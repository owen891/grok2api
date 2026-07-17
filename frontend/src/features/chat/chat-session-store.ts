import {
  CHAT_STORE_KEY,
  createEmptySession,
  isSupportedImageUrl,
  isUsageLimitError,
  type ChatMessage,
  type ChatPrefs,
  type ChatSession,
  type ImageSettings,
  DEFAULT_IMAGE_SETTINGS,
  normalizeImageSettings,
  sanitizeGenerationMeta,
  sanitizePersistedMessageTask,
} from "./chat-types";

const maxPersistedSessions = 20;
const maxPersistedMessages = 100;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function clampN(value: unknown): number {
  const n = typeof value === "number" ? value : Number(value);
  if (!Number.isFinite(n)) return 1;
  return Math.min(4, Math.max(1, Math.round(n)));
}

function sanitizeImageSettings(raw: unknown): ImageSettings {
  if (!isRecord(raw)) return { ...DEFAULT_IMAGE_SETTINGS };
  const quality = raw.quality === "quality" ? "quality" : "speed";
  const aspectRatio = typeof raw.aspectRatio === "string" && raw.aspectRatio ? raw.aspectRatio : DEFAULT_IMAGE_SETTINGS.aspectRatio;
  const resolution = raw.resolution === "2k" ? "2k" : "1k";
  return normalizeImageSettings({
    quality,
    n: clampN(raw.n),
    aspectRatio,
    resolution,
  });
}

function sanitizeSession(raw: unknown): ChatSession | null {
  if (!isRecord(raw) || typeof raw.id !== "string") return null;
  const mode = raw.mode === "image" ? "image" : "chat";
  const messages: ChatMessage[] = Array.isArray(raw.messages)
    ? raw.messages
        .slice(-maxPersistedMessages)
        .filter(isRecord)
        .map((message) => {
          const role: ChatMessage["role"] =
            message.role === "assistant" || message.role === "system" ? message.role : "user";
          const images = Array.isArray(message.images)
            ? message.images
                .filter(isRecord)
                .map((image) => ({
                  url: typeof image.url === "string" ? image.url.trim() : "",
                  previewUrl: typeof image.previewUrl === "string" ? image.previewUrl.trim() : undefined,
                  mimeType: typeof image.mimeType === "string" ? image.mimeType : undefined,
                }))
                .filter((image) => isSupportedImageUrl(image.url))
            : undefined;
          const errorClass =
            isRecord(message.error) &&
            (message.error.class === "auth" ||
              message.error.class === "account" ||
              message.error.class === "model" ||
              message.error.class === "quota" ||
              message.error.class === "egress" ||
              message.error.class === "upstream" ||
              message.error.class === "timeout" ||
              message.error.class === "rate")
              ? message.error.class
              : "unknown";
          const errorCode = isRecord(message.error) && typeof message.error.code === "string"
            ? message.error.code
            : undefined;
          const errorMessage = isRecord(message.error) && typeof message.error.message === "string"
            ? message.error.message
            : "Unknown error";
          const usageLimitReached = isUsageLimitError(errorCode, errorMessage);
          const result: ChatMessage = {
            id: typeof message.id === "string" ? message.id : `msg_${Date.now()}`,
            role,
            content: typeof message.content === "string" ? message.content : "",
            images,
            createdAt: typeof message.createdAt === "number" ? message.createdAt : Date.now(),
            task: sanitizePersistedMessageTask(message.task),
            generation: sanitizeGenerationMeta(message.generation),
            error: isRecord(message.error)
              ? {
                  class: usageLimitReached ? "quota" : errorClass,
                  message: usageLimitReached ? "上游账号额度不足或正在等待恢复，请稍后重试。" : errorMessage,
                  code: errorCode,
                  requestId: typeof message.error.requestId === "string" ? message.error.requestId : undefined,
                }
              : undefined,
          };
          return result;
        })
    : [];

  return {
    id: raw.id,
    title: typeof raw.title === "string" ? raw.title : "",
    mode,
    model: typeof raw.model === "string" ? raw.model : "",
    imageSettings: sanitizeImageSettings(raw.imageSettings),
    messages,
    createdAt: typeof raw.createdAt === "number" ? raw.createdAt : Date.now(),
    updatedAt: typeof raw.updatedAt === "number" ? raw.updatedAt : Date.now(),
  };
}

export function loadChatPrefs(): ChatPrefs {
  if (typeof window === "undefined") {
    return emptyPrefs();
  }
  try {
    const raw = window.localStorage.getItem(CHAT_STORE_KEY);
    if (!raw) return emptyPrefs();
    const parsed: unknown = JSON.parse(raw);
    if (!isRecord(parsed) || parsed.version !== 1) return emptyPrefs();
    const sessions = Array.isArray(parsed.sessions)
      ? parsed.sessions
          .slice(0, maxPersistedSessions)
          .map(sanitizeSession)
          .filter((session): session is ChatSession => Boolean(session))
      : [];
    const activeSessionId =
      typeof parsed.activeSessionId === "string" && sessions.some((session) => session.id === parsed.activeSessionId)
        ? parsed.activeSessionId
        : sessions[0]?.id ?? null;
    return {
      version: 1,
      clientKeyId: typeof parsed.clientKeyId === "string" ? parsed.clientKeyId : null,
      activeSessionId,
      sessions,
    };
  } catch {
    return emptyPrefs();
  }
}

export function saveChatPrefs(prefs: ChatPrefs): void {
  if (typeof window === "undefined") return;
  const payload: ChatPrefs = {
    version: 1,
    clientKeyId: prefs.clientKeyId,
    activeSessionId: prefs.activeSessionId,
    sessions: prefs.sessions.slice(0, maxPersistedSessions).map((session) => ({
      ...session,
      messages: session.messages.slice(-maxPersistedMessages).map((message) => {
        const persisted = { ...message };
        delete persisted.streaming;
        persisted.images = message.images
          ?.map((image) => {
            if (!/^data:/i.test(image.url)) return image;
            const previewUrl = image.previewUrl && isSupportedImageUrl(image.previewUrl) ? image.previewUrl : "";
            return previewUrl ? { ...image, url: previewUrl, previewUrl } : null;
          })
          .filter((image): image is NonNullable<typeof image> => Boolean(image && isSupportedImageUrl(image.url)));
        if (persisted.images?.length === 0) delete persisted.images;
        return persisted;
      }),
    })),
  };
  try {
    window.localStorage.setItem(CHAT_STORE_KEY, JSON.stringify(payload));
  } catch {
    // Session persistence is best effort; exhausted browser storage must not break chat.
  }
}

export function emptyPrefs(): ChatPrefs {
  const session = createEmptySession();
  return {
    version: 1,
    clientKeyId: null,
    activeSessionId: session.id,
    sessions: [session],
  };
}

export function ensureActiveSession(prefs: ChatPrefs): ChatPrefs {
  if (prefs.sessions.length === 0) {
    const session = createEmptySession();
    return { ...prefs, sessions: [session], activeSessionId: session.id };
  }
  if (!prefs.activeSessionId || !prefs.sessions.some((session) => session.id === prefs.activeSessionId)) {
    return { ...prefs, activeSessionId: prefs.sessions[0].id };
  }
  return prefs;
}
