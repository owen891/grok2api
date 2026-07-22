export type ChatMode = "chat" | "image";

export type ImageQuality = "speed" | "quality";

export type ChatErrorClass =
  | "auth"
  | "account"
  | "model"
  | "quota"
  | "moderation"
  | "imagePermission"
  | "egress"
  | "upstream"
  | "timeout"
  | "rate"
  | "unknown";

export type ImageSettings = {
  quality: ImageQuality;
  n: number;
  aspectRatio: string;
  resolution: string;
};

export type ChatImageRef = {
  url: string;
  previewUrl?: string;
  mimeType?: string;
};

export type MessageGenerationMeta = {
  mode: ChatMode;
  model: string;
  clientKeyId?: string;
  clientKeyName?: string;
  clientKeyPrefix?: string;
  n?: number;
  aspectRatio?: string;
  resolution?: string;
  quality?: ImageQuality;
};

export type MessageTask = {
  kind: "chat" | "image";
  status: "queued" | "running" | "completed" | "failed" | "cancelled";
  requestId?: string;
  progress?: number;
};

export type ChatMessage = {
  id: string;
  role: "user" | "assistant" | "system";
  content: string;
  images?: ChatImageRef[];
  createdAt: number;
  streaming?: boolean;
  task?: MessageTask;
  generation?: MessageGenerationMeta;
  error?: {
    class: ChatErrorClass;
    message: string;
    code?: string;
    requestId?: string;
  };
};

export function isUsageLimitError(code?: string, message?: string): boolean {
  return /usage[_ -]?limit|图片额度.*用完/i.test(`${code ?? ""} ${message ?? ""}`);
}

export function sanitizePersistedMessageTask(raw: unknown): MessageTask | undefined {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return undefined;
  const value = raw as Record<string, unknown>;
  if (value.kind !== "chat" && value.kind !== "image") return undefined;
  const statuses: MessageTask["status"][] = ["queued", "running", "completed", "failed", "cancelled"];
  if (!statuses.includes(value.status as MessageTask["status"])) return undefined;
  const requestId = typeof value.requestId === "string" && value.requestId.trim()
    ? value.requestId.trim()
    : undefined;
  const progress = typeof value.progress === "number" && Number.isFinite(value.progress)
    ? Math.min(100, Math.max(0, Math.round(value.progress)))
    : undefined;
  const interrupted =
    (value.status === "queued" || value.status === "running") &&
    (value.kind === "chat" || !requestId);
  return {
    kind: value.kind,
    status: interrupted ? "cancelled" : value.status as MessageTask["status"],
    requestId,
    progress,
  };
}

export function sanitizeGenerationMeta(raw: unknown): MessageGenerationMeta | undefined {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return undefined;
  const value = raw as Record<string, unknown>;
  if (value.mode !== "chat" && value.mode !== "image") return undefined;
  const count = typeof value.n === "number" && Number.isFinite(value.n)
    ? Math.min(4, Math.max(1, Math.round(value.n)))
    : undefined;
  return {
    mode: value.mode,
    model: typeof value.model === "string" ? value.model.trim() : "",
    clientKeyId: typeof value.clientKeyId === "string" ? value.clientKeyId : undefined,
    clientKeyName: typeof value.clientKeyName === "string" ? value.clientKeyName.trim() : undefined,
    clientKeyPrefix: typeof value.clientKeyPrefix === "string" ? value.clientKeyPrefix.trim() : undefined,
    n: count,
    aspectRatio: typeof value.aspectRatio === "string" ? value.aspectRatio : undefined,
    resolution: typeof value.resolution === "string" ? value.resolution : undefined,
    quality: value.quality === "quality" || value.quality === "speed" ? value.quality : undefined,
  };
}

export type ChatSession = {
  id: string;
  title: string;
  mode: ChatMode;
  model: string;
  imageSettings: ImageSettings;
  messages: ChatMessage[];
  createdAt: number;
  updatedAt: number;
};

export type ChatPrefs = {
  version: 1;
  clientKeyId: string | null;
  activeSessionId: string | null;
  sessions: ChatSession[];
};

export const CHAT_STORE_KEY = "grok2api_chat_sessions_v1";

export const DEFAULT_IMAGE_SETTINGS: ImageSettings = {
  quality: "speed",
  n: 1,
  aspectRatio: "1:1",
  resolution: "1k",
};

export const SPEED_IMAGE_MODEL = "grok-imagine-image";
export const QUALITY_IMAGE_MODEL = "grok-imagine-image-quality";

export const ASPECT_RATIO_OPTIONS = ["1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2"] as const;
export const RESOLUTION_OPTIONS = ["1k", "2k"] as const;

export function imageModelForQuality(quality: ImageQuality): string {
  return quality === "quality" ? QUALITY_IMAGE_MODEL : SPEED_IMAGE_MODEL;
}

export function imageSettingsRequireQuality(
  settings: Pick<ImageSettings, "aspectRatio" | "resolution">,
): boolean {
  return settings.aspectRatio !== "1:1" || settings.resolution !== "1k";
}

export function normalizeImageSettings(settings: ImageSettings): ImageSettings {
  if (!imageSettingsRequireQuality(settings) || settings.quality === "quality") return settings;
  return { ...settings, quality: "quality" };
}

export function imageSettingsForModel(settings: ImageSettings, model: string): ImageSettings {
  if (/quality/i.test(model)) return { ...settings, quality: "quality" };
  return { ...settings, quality: "speed", aspectRatio: "1:1", resolution: "1k" };
}

export function imageSettingsForAvailableModels(settings: ImageSettings, models: string[]): ImageSettings {
  const normalized = normalizeImageSettings(settings);
  const qualityAvailable = models.some((model) => /quality/i.test(model) && !/edit/i.test(model));
  return normalized.quality === "quality" && !qualityAvailable
    ? imageSettingsForModel(normalized, SPEED_IMAGE_MODEL)
    : normalized;
}

export function isSupportedImageUrl(url: string): boolean {
  const value = url.trim();
  if (!value) return false;
  if (/^data:image\//i.test(value)) return true;
  if (/^(?:https?:|blob:)/i.test(value)) return true;
  if (value.startsWith("//")) return false;
  return !/^[a-z][a-z\d+.-]*:/i.test(value);
}

export function createId(prefix: string): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return `${prefix}_${crypto.randomUUID()}`;
  }
  return `${prefix}_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 10)}`;
}

export function createEmptySession(partial?: Partial<ChatSession>): ChatSession {
  const now = Date.now();
  return {
    id: createId("sess"),
    title: "新会话",
    mode: "chat",
    model: "",
    imageSettings: { ...DEFAULT_IMAGE_SETTINGS },
    messages: [],
    createdAt: now,
    updatedAt: now,
    ...partial,
  };
}

export function defaultTitle(locale: "zh" | "en", index: number): string {
  return locale === "zh" ? `会话 ${index}` : `Session ${index}`;
}

export function displayModelName(modelId: string): string {
  const id = modelId.trim();
  if (!id) return "未选择模型";
  if (/imagine-image-edit/i.test(id)) return "图生图";
  if (/imagine-image-quality/i.test(id)) return "高质量生图";
  if (/imagine-image/i.test(id)) return "快速生图";
  if (/chat-fast/i.test(id)) return "快速对话";
  if (/chat-expert/i.test(id)) return "专家对话";
  if (/^grok-4\.5$/i.test(id)) return "对话 · Grok 4.5";
  if (/^grok-4\.20/i.test(id)) return "对话 · Grok 4.20";
  if (/imagine|image/i.test(id)) return `生图 · ${id}`;
  if (id.startsWith("grok-")) return `对话 · ${id}`;
  return id;
}

export function formatGenerationMeta(meta?: MessageGenerationMeta): string {
  if (!meta) return "";
  const key = meta.clientKeyName
    ? `${meta.clientKeyName}${meta.clientKeyPrefix ? ` (${meta.clientKeyPrefix}...)` : ""}`
    : "";
  if (meta.mode === "image") {
    const parts = [
      displayModelName(meta.model),
      meta.aspectRatio,
      meta.resolution,
      typeof meta.n === "number" ? `${meta.n}张` : undefined,
      meta.quality === "quality" ? "高质量" : meta.quality === "speed" ? "快速" : undefined,
    ].filter(Boolean);
    if (key) parts.push(`密钥 · ${key}`);
    return parts.join(" · ");
  }
  return [displayModelName(meta.model), key ? `密钥 · ${key}` : ""].filter(Boolean).join(" · ");
}

export function deriveSessionTitle(prompt: string, fallback = "新会话"): string {
  const compact = prompt.replace(/\s+/g, " ").trim();
  if (!compact) return fallback;
  return compact.length > 24 ? `${compact.slice(0, 24)}…` : compact;
}

export function isDefaultSessionTitle(title: string, fallbackTitle?: string): boolean {
  const value = title.trim();
  if (!value) return true;
  if (fallbackTitle && value === fallbackTitle) return true;
  return (
    value === "新会话" ||
    value === "New chat" ||
    value === "Untitled" ||
    /^会话\s*\d+$/.test(value) ||
    /^Chat\s*\d+$/i.test(value) ||
    /^未命名/.test(value)
  );
}

export function titleWithPrompt(currentTitle: string, prompt: string, fallbackTitle = "新会话"): string {
  if (!isDefaultSessionTitle(currentTitle, fallbackTitle)) return currentTitle;
  return deriveSessionTitle(prompt, fallbackTitle);
}
