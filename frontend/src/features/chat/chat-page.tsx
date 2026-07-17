import { useQuery } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Loader2, Menu, MessageSquarePlus, Trash2, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import {
  getClientKeySecret,
  listClientKeys,
  type ClientKeyDTO,
} from "@/features/client-keys/client-keys-api";
import { listModels } from "@/entities/model/model-api";
import type { ModelRouteDTO } from "@/entities/model/types";
import {
  createImageJob,
  listGatewayModels,
  streamChatCompletion,
  toClassifiedError,
  waitForImageJob,
  type ClassifiedError,
  type ChatCompletionMessage,
} from "./chat-api";
import { Composer, OPEN_IMAGE_SETTINGS_EVENT } from "./composer";
import { MessageList } from "./message-list";
import { ensureActiveSession, loadChatPrefs, saveChatPrefs } from "./chat-session-store";
import {
  createEmptySession,
  createId,
  defaultTitle,
  displayModelName,
  imageSettingsForAvailableModels,
  imageModelForQuality,
  SPEED_IMAGE_MODEL,
  titleWithPrompt,
  type ChatMessage,
  type ChatImageRef,
  type ChatMode,
  type ChatPrefs,
  type ChatSession,
  type ImageSettings,
  type MessageGenerationMeta,
} from "./chat-types";

type ActiveTaskController = {
  controller: AbortController;
  sessionId: string;
  kind: "chat" | "image";
};

const emptyClientKeys: ClientKeyDTO[] = [];
const emptyAdminModels: ModelRouteDTO[] = [];
const emptyGatewayModelIds: string[] = [];

function isChatCapable(model: ModelRouteDTO): boolean {
  if (!model.enabled) return false;
  if (model.capability === "image" || model.capability === "image_edit" || model.capability === "video") {
    return false;
  }
  return model.capability === "chat" || model.capability === "responses";
}


async function loadAllModels(): Promise<ModelRouteDTO[]> {
  const pageSize = 100;
  let page = 1;
  let total = Number.POSITIVE_INFINITY;
  const items: ModelRouteDTO[] = [];
  while (items.length < total && page <= 20) {
    const result = await listModels({ page, pageSize });
    items.push(...result.items.filter((m) => m.enabled));
    total = result.total;
    if (result.items.length === 0) break;
    page += 1;
  }
  return items;
}

async function loadActiveClientKeys(): Promise<ClientKeyDTO[]> {
  const pageSize = 100;
  let page = 1;
  let total = Number.POSITIVE_INFINITY;
  const items: ClientKeyDTO[] = [];
  while (items.length < total && page <= 20) {
    const result = await listClientKeys({ page, pageSize });
    items.push(...result.items);
    total = result.total;
    if (result.items.length === 0) break;
    page += 1;
  }
  return items.filter((key) => key.enabled);
}

function pickImageModel(imageModels: string[], quality: "speed" | "quality"): string {
  if (quality === "quality") {
    return imageModels.find((id) => /quality/i.test(id) && !/edit/i.test(id)) || imageModels.find((id) => !/edit/i.test(id)) || imageModelForQuality("quality");
  }
  return imageModels.find((id) => /imagine|image/i.test(id) && !/quality|edit/i.test(id)) || imageModels.find((id) => !/edit/i.test(id)) || imageModelForQuality("speed");
}

function toChatCompletionMessage(message: ChatMessage): ChatCompletionMessage {
  if (message.role === "user" && message.images?.length) {
    return {
      role: "user",
      content: [
        ...(message.content ? [{ type: "text" as const, text: message.content }] : []),
        ...message.images.map((image) => ({
          type: "image_url" as const,
          image_url: { url: image.url },
        })),
      ],
    };
  }
  return { role: message.role, content: message.content };
}

export function ChatPage() {
  const { t, i18n } = useTranslation();
  const locale = i18n.language.toLowerCase().startsWith("zh") ? "zh" : "en";
  const [prefs, setPrefs] = useState<ChatPrefs>(() => ensureActiveSession(loadChatPrefs()));
  const [draft, setDraft] = useState("");
  const [draftImages, setDraftImages] = useState<ChatImageRef[]>([]);
  const [pendingDelete, setPendingDelete] = useState<
    null | { type: "message"; messageId: string } | { type: "session"; sessionId: string }
  >(null);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const taskControllersRef = useRef(new Map<string, ActiveTaskController>());
  const threadRef = useRef<HTMLDivElement | null>(null);

  const activeSession = useMemo(() => {
    return prefs.sessions.find((session) => session.id === prefs.activeSessionId) ?? prefs.sessions[0];
  }, [prefs]);

  const metadataQuery = useQuery({
    queryKey: ["chat", "metadata"],
    queryFn: async () => {
      const [clientKeys, adminModels] = await Promise.all([loadActiveClientKeys(), loadAllModels()]);
      return { clientKeys, adminModels };
    },
  });
  const clientKeys = metadataQuery.data?.clientKeys ?? emptyClientKeys;
  const adminModels = metadataQuery.data?.adminModels ?? emptyAdminModels;
  const selectedClientKey = useMemo(
    () => clientKeys.find((key) => key.id === prefs.clientKeyId) ?? null,
    [clientKeys, prefs.clientKeyId],
  );
  const effectiveClientKeyId = selectedClientKey?.id ?? null;

  const clientSecretQuery = useQuery({
    queryKey: ["chat", "client-secret", effectiveClientKeyId],
    queryFn: () => getClientKeySecret(effectiveClientKeyId as string),
    enabled: Boolean(selectedClientKey),
  });
  const clientSecret = clientSecretQuery.data?.secret ?? null;

  const gatewayModelsQuery = useQuery({
    queryKey: ["chat", "gateway-models", effectiveClientKeyId],
    queryFn: ({ signal }) => listGatewayModels(clientSecret as string, signal),
    enabled: Boolean(clientSecret),
  });
  const gatewayModelIds = gatewayModelsQuery.data?.map((model) => model.id) ?? emptyGatewayModelIds;
  const loadingMeta = metadataQuery.isPending;
  const loadingSecret = clientSecretQuery.isFetching;
  const metaError = metadataQuery.error instanceof Error
    ? metadataQuery.error.message
    : metadataQuery.error
      ? t("chat.metaLoadFailed")
      : null;

  const chatModels = useMemo(() => {
    const gatewayChat = gatewayModelIds.filter((id) => !/imagine|image|video/i.test(id));
    if (gatewayChat.length > 0) return gatewayChat;
    return adminModels.filter(isChatCapable).map((model) => model.publicId);
  }, [adminModels, gatewayModelIds]);

  const imageModels = useMemo(() => {
    const gatewayImage = gatewayModelIds.filter((id) => /imagine|image/i.test(id) && !/video|edit/i.test(id));
    return Array.from(new Set([...gatewayImage, SPEED_IMAGE_MODEL]));
  }, [gatewayModelIds]);

  const activeImageSettings = useMemo(
    () => imageSettingsForAvailableModels(activeSession.imageSettings, imageModels),
    [activeSession.imageSettings, imageModels],
  );

  const activeModel = useMemo(() => {
    if (!activeSession) return "";
    if (activeSession.mode === "image") {
      if (activeSession.model && imageModels.includes(activeSession.model)) return activeSession.model;
      return pickImageModel(imageModels, activeImageSettings.quality);
    }
    if (activeSession.model && chatModels.includes(activeSession.model)) return activeSession.model;
    return chatModels[0] ?? "";
  }, [activeImageSettings.quality, activeSession, chatModels, imageModels]);

  useEffect(() => {
    const hasResumableImageTask = prefs.sessions.some((session) =>
      session.messages.some((message) =>
        message.task?.kind === "image" &&
        (message.task.status === "queued" || message.task.status === "running") &&
        Boolean(message.task.requestId),
      ),
    );
    if (hasResumableImageTask) {
      saveChatPrefs(prefs);
      return;
    }
    const timer = window.setTimeout(() => saveChatPrefs(prefs), 400);
    return () => window.clearTimeout(timer);
  }, [prefs]);

  useEffect(() => {
    const node = threadRef.current;
    if (!node) return;
    node.scrollTop = node.scrollHeight;
  }, [activeSession?.messages]);

  const updateSession = useCallback((sessionId: string, updater: (session: ChatSession) => ChatSession) => {
    setPrefs((prev) => ({
      ...prev,
      sessions: prev.sessions.map((session) => (session.id === sessionId ? updater(session) : session)),
    }));
  }, []);

  useEffect(() => {
    if (clientSecretQuery.error) {
      toast.error(clientSecretQuery.error instanceof Error ? clientSecretQuery.error.message : t("chat.keySecretFailed"));
    }
  }, [clientSecretQuery.error, t]);

  const onCreateSession = () => {
    const index = prefs.sessions.length + 1;
    const session = createEmptySession({
      title: defaultTitle(locale, index),
      model: chatModels[0] ?? "",
    });
    setPrefs((prev) => ({
      ...prev,
      sessions: [session, ...prev.sessions],
      activeSessionId: session.id,
    }));
    setDraft("");
    setDraftImages([]);
  };

  const onDeleteSession = (sessionId: string) => {
    for (const task of taskControllersRef.current.values()) {
      if (task.sessionId === sessionId) task.controller.abort();
    }
    setPrefs((prev) => {
      const remaining = prev.sessions.filter((session) => session.id !== sessionId);
      if (remaining.length === 0) {
        const session = createEmptySession({
          title: defaultTitle(locale, 1),
          model: chatModels[0] ?? "",
        });
        return { ...prev, sessions: [session], activeSessionId: session.id };
      }
      const activeSessionId = prev.activeSessionId === sessionId ? remaining[0].id : prev.activeSessionId;
      return { ...prev, sessions: remaining, activeSessionId };
    });
  };

  const appendMessages = useCallback((sessionId: string, messages: ChatMessage[]) => {
    updateSession(sessionId, (session) => ({
      ...session,
      messages: [...session.messages, ...messages],
      updatedAt: Date.now(),
    }));
  }, [updateSession]);

  const patchMessage = useCallback((sessionId: string, messageId: string, patch: Partial<ChatMessage>) => {
    updateSession(sessionId, (session) => ({
      ...session,
      messages: session.messages.map((message) => (message.id === messageId ? { ...message, ...patch } : message)),
      updatedAt: Date.now(),
    }));
  }, [updateSession]);

  const stopTask = useCallback((messageId: string) => {
    taskControllersRef.current.get(messageId)?.controller.abort();
  }, []);

  const requestDeleteMessage = (messageId: string) => {
    setPendingDelete({ type: "message", messageId });
  };

  const requestDeleteSession = (sessionId: string) => {
    setPendingDelete({ type: "session", sessionId });
  };

  const confirmPendingDelete = () => {
    if (!pendingDelete) return;
    if (pendingDelete.type === "message") {
      if (!activeSession) {
        setPendingDelete(null);
        return;
      }
      const messageId = pendingDelete.messageId;
      stopTask(messageId);
      updateSession(activeSession.id, (session) => ({
        ...session,
        messages: session.messages.filter((message) => message.id !== messageId),
        updatedAt: Date.now(),
      }));
    } else {
      onDeleteSession(pendingDelete.sessionId);
    }
    setPendingDelete(null);
  };

  const fillPrompt = (prompt: string) => {
    setDraft(prompt);
    toast.message("已填入输入框");
  };

  const openImageSettings = () => {
    if (activeSession && activeSession.mode !== "image") {
      updateSession(activeSession.id, (session) => ({
        ...session,
        mode: "image",
        model: pickImageModel(imageModels, session.imageSettings.quality),
        updatedAt: Date.now(),
      }));
    }
    window.setTimeout(() => {
      window.dispatchEvent(new Event(OPEN_IMAGE_SETTINGS_EVENT));
    }, 0);
  };

  const pollImageTask = useCallback(async (options: {
    sessionId: string;
    messageId: string;
    requestId: string;
    secret: string;
    controller: AbortController;
  }) => {
    const { sessionId, messageId, requestId, secret, controller } = options;
    try {
      const images = await waitForImageJob({
        clientKey: secret,
        requestId,
        signal: controller.signal,
        onProgress: (progress) => patchMessage(sessionId, messageId, {
          task: { kind: "image", status: "running", requestId, progress },
        }),
      });
      if (images.length === 0) {
        throw { class: "upstream", message: t("chat.image.empty"), requestId } satisfies ClassifiedError;
      }
      patchMessage(sessionId, messageId, {
        content: t("chat.image.done", { count: images.length }),
        images: images.map((image) => ({ url: image.url, mimeType: image.mimeType })),
        streaming: false,
        task: { kind: "image", status: "completed", requestId, progress: 100 },
      });
    } catch (error) {
      if (controller.signal.aborted) {
        patchMessage(sessionId, messageId, {
          streaming: false,
          content: t("chat.image.cancelled"),
          task: { kind: "image", status: "cancelled", requestId },
        });
      } else {
        const classified = toClassifiedError(error, t("chat.error.unknown"));
        toast.error(classified.message || t("chat.error.unknown"));
        patchMessage(sessionId, messageId, {
          streaming: false,
          content: classified.message || t("chat.error.unknown"),
          task: { kind: "image", status: "failed", requestId },
          error: {
            class: classified.class,
            message: classified.message,
            code: classified.code,
            requestId: classified.requestId || requestId,
          },
        });
      }
    } finally {
      const active = taskControllersRef.current.get(messageId);
      if (active?.controller === controller) taskControllersRef.current.delete(messageId);
    }
  }, [patchMessage, t]);

  useEffect(() => {
    if (!clientSecret || !selectedClientKey) return;
    for (const session of prefs.sessions) {
      for (const message of session.messages) {
        const task = message.task;
        if (
          task?.kind !== "image" ||
          (task.status !== "queued" && task.status !== "running") ||
          !task.requestId ||
          taskControllersRef.current.has(message.id)
        ) {
          continue;
        }
        if (message.generation?.clientKeyId && message.generation.clientKeyId !== selectedClientKey.id) continue;
        const controller = new AbortController();
        taskControllersRef.current.set(message.id, { controller, sessionId: session.id, kind: "image" });
        queueMicrotask(() => {
          void pollImageTask({
            sessionId: session.id,
            messageId: message.id,
            requestId: task.requestId as string,
            secret: clientSecret,
            controller,
          });
        });
      }
    }
  }, [clientSecret, pollImageTask, prefs.sessions, selectedClientKey]);

  const send = async (promptOverride?: string, forcedMode?: ChatMode, attachmentOverride?: ChatImageRef[]) => {
    if (!activeSession) return;
    const mode = forcedMode ?? activeSession.mode;
    const attachments = mode === "chat" ? (attachmentOverride ?? (promptOverride === undefined ? draftImages : [])) : [];
    const prompt = (promptOverride ?? draft).trim();
    if (!prompt && attachments.length === 0) return;
    if (!effectiveClientKeyId) {
      toast.error(t("chat.needClientKey"));
      return;
    }
    if (!clientSecret) {
      toast.error(t("chat.needClientSecret"));
      return;
    }

    const sessionId = activeSession.id;
    if (
      mode === "chat" &&
      Array.from(taskControllersRef.current.values()).some((task) => task.kind === "chat" && task.sessionId === sessionId)
    ) {
      toast.message("当前会话已有一条回复正在生成");
      return;
    }
    const userMessage: ChatMessage = {
      id: createId("msg"),
      role: "user",
      content: prompt,
      images: attachments.length > 0 ? attachments : undefined,
      createdAt: Date.now(),
    };
    const assistantId = createId("msg");
    const imageSettings = imageSettingsForAvailableModels(activeSession.imageSettings, imageModels);
    const model =
      mode === "image"
        ? activeSession.model && imageModels.includes(activeSession.model)
          ? activeSession.model
          : pickImageModel(imageModels, imageSettings.quality)
        : activeModel;
    const generation: MessageGenerationMeta =
      mode === "image"
        ? {
            mode,
            model,
            clientKeyId: selectedClientKey?.id,
            clientKeyName: selectedClientKey?.name,
            clientKeyPrefix: selectedClientKey?.prefix,
            n: imageSettings.n,
            aspectRatio: imageSettings.aspectRatio,
            resolution: imageSettings.resolution,
            quality: imageSettings.quality,
          }
        : {
            mode,
            model,
            clientKeyId: selectedClientKey?.id,
            clientKeyName: selectedClientKey?.name,
            clientKeyPrefix: selectedClientKey?.prefix,
          };

    const assistantMessage: ChatMessage = {
      id: assistantId,
      role: "assistant",
      content: "",
      createdAt: Date.now(),
      streaming: true,
      task: { kind: mode, status: mode === "image" ? "queued" : "running", progress: 0 },
      generation,
    };

    setDraft("");
    if (mode === "chat") setDraftImages([]);
    appendMessages(sessionId, [userMessage, assistantMessage]);
    updateSession(sessionId, (session) => ({
      ...session,
      title: titleWithPrompt(session.title, prompt || "图片对话", defaultTitle(locale, prefs.sessions.length || 1)),
      updatedAt: Date.now(),
    }));

    const controller = new AbortController();
    taskControllersRef.current.set(assistantId, { controller, sessionId, kind: mode });

    try {
      if (mode === "image") {
        toast.message("图片任务已提交，可继续对话");
        // 强制使用生图模型，避免会话里残留的对话模型（如 grok-4.5）被误发到 /v1/images
        const imageModel = model;
        if (activeSession.model !== imageModel || activeSession.imageSettings !== imageSettings) {
          updateSession(sessionId, (session) => ({
            ...session,
            model: imageModel,
            imageSettings,
            updatedAt: Date.now(),
          }));
        }
        const { requestId } = await createImageJob({
          clientKey: clientSecret,
          prompt,
          settings: imageSettings,
          model: imageModel,
          signal: controller.signal,
        });
        patchMessage(sessionId, assistantId, {
          task: { kind: "image", status: "running", requestId, progress: 0 },
        });
        await pollImageTask({ sessionId, messageId: assistantId, requestId, secret: clientSecret, controller });
      } else {
        const model = activeModel;
        if (!model) {
          throw { class: "unknown", message: t("chat.controls.noModels") } satisfies ClassifiedError;
        }
        const history = [...activeSession.messages, userMessage]
          .filter((message) => message.role === "user" || message.role === "assistant")
          .filter((message) => message.content || message.images?.length)
          .map(toChatCompletionMessage);
        await streamChatCompletion({
          clientKey: clientSecret,
          model,
          messages: history,
          signal: controller.signal,
          onDelta: (piece) => {
            setPrefs((prev) => ({
              ...prev,
              sessions: prev.sessions.map((session) => {
                if (session.id !== sessionId) return session;
                return {
                  ...session,
                  updatedAt: Date.now(),
                  messages: session.messages.map((message) =>
                    message.id === assistantId
                      ? { ...message, content: `${message.content}${piece}`, streaming: true, task: { kind: "chat", status: "running" } }
                      : message,
                  ),
                };
              }),
            }));
          },
        });
        patchMessage(sessionId, assistantId, {
          streaming: false,
          task: { kind: "chat", status: "completed", progress: 100 },
        });
      }
    } catch (error) {
      if (mode === "image" && !taskControllersRef.current.has(assistantId)) return;
      if (controller.signal.aborted) {
        patchMessage(sessionId, assistantId, {
          streaming: false,
          content: mode === "image" ? t("chat.image.cancelled") : t("chat.chat.cancelled"),
          task: { kind: mode, status: "cancelled" },
        });
      } else {
        const classified = toClassifiedError(error, t("chat.error.unknown"));
        toast.error(classified.message || t("chat.error.unknown"));
        patchMessage(sessionId, assistantId, {
          streaming: false,
          content: classified.message || t("chat.error.unknown"),
          task: { kind: mode, status: "failed" },
          error: {
            class: classified.class,
            message: classified.message,
            code: classified.code,
            requestId: classified.requestId,
          },
        });
      }
    } finally {
      const active = taskControllersRef.current.get(assistantId);
      if (active?.controller === controller) taskControllersRef.current.delete(assistantId);
    }
  };

  const activeChatMessage = [...activeSession.messages].reverse().find((message) =>
    message.task?.kind === "chat" &&
    (message.task.status === "queued" || message.task.status === "running"),
  );
  const chatSending = activeSession.mode === "chat" && Boolean(activeChatMessage);

  if (!activeSession) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">{t("chat.empty")}</div>
    );
  }

  return (
    <>
    <div className="flex h-[calc(100vh-5.5rem)] min-h-[560px] w-full flex-col">
      <div className="relative flex h-full min-h-0 overflow-hidden rounded-2xl border border-border/60 bg-gradient-to-br from-background via-background to-muted/20 shadow-sm">
        {sidebarOpen ? (
          <button
            type="button"
            className="absolute inset-0 z-20 bg-black/40 lg:hidden"
            aria-label="关闭会话列表"
            onClick={() => setSidebarOpen(false)}
          />
        ) : null}

        <aside
          className={`absolute inset-y-0 left-0 z-30 flex w-[min(86vw,18rem)] flex-col border-r border-border/50 bg-card/95 backdrop-blur transition-transform duration-200 lg:static lg:z-0 lg:w-72 ${
            sidebarOpen ? "translate-x-0" : "-translate-x-full lg:translate-x-0"
          }`}
        >
          <div className="flex items-center justify-between px-4 py-3">
            <div>
              <div className="text-sm font-semibold tracking-tight">{t("chat.sessions")}</div>
              <div className="text-[11px] text-muted-foreground">本地保存，不存密钥</div>
            </div>
            <div className="flex items-center gap-1">
              <Button type="button" size="sm" variant="secondary" className="rounded-full" onClick={onCreateSession}>
                <MessageSquarePlus className="h-4 w-4" />
              </Button>
              <Button type="button" size="sm" variant="ghost" className="rounded-full lg:hidden" onClick={() => setSidebarOpen(false)}>
                <X className="h-4 w-4" />
              </Button>
            </div>
          </div>
          <div className="min-h-0 flex-1 space-y-1 overflow-y-auto px-2 pb-3">
            {prefs.sessions.map((session) => {
              const active = session.id === activeSession.id;
              return (
                <div
                  key={session.id}
                  className={`group flex items-center gap-1 rounded-xl px-2.5 py-2.5 text-sm transition ${
                    active ? "bg-primary/12 text-primary shadow-sm" : "hover:bg-muted/70"
                  }`}
                >
                  <button
                    type="button"
                    className="min-w-0 flex-1 truncate text-left"
                    onClick={() => {
                      setPrefs((prev) => ({ ...prev, activeSessionId: session.id }));
                      setDraftImages([]);
                      setSidebarOpen(false);
                    }}
                  >
                    {session.title || t("chat.untitled")}
                    <div className="text-[11px] opacity-60">{session.mode === "image" ? "生图" : "对话"}</div>
                  </button>
                  <button
                    type="button"
                    className="rounded p-1 opacity-0 transition group-hover:opacity-100 hover:bg-background"
                    onClick={() => requestDeleteSession(session.id)}
                    aria-label={t("chat.deleteSession")}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </button>
                </div>
              );
            })}
          </div>
          <div className="border-t border-border/50 p-3">
            <label className="mb-1 block text-[11px] font-medium text-muted-foreground">{t("chat.clientKey")}</label>
            <select
              value={effectiveClientKeyId ?? ""}
              disabled={loadingMeta}
              onChange={(event) =>
                setPrefs((prev) => ({
                  ...prev,
                  clientKeyId: event.target.value || null,
                }))
              }
              className="h-9 w-full rounded-lg border border-border/60 bg-background/80 px-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              <option value="">{t("chat.clientKeyPlaceholder")}</option>
              {clientKeys.map((key) => (
                <option key={key.id} value={key.id}>
                  {key.name} ({key.prefix}…)
                </option>
              ))}
            </select>
            {clientKeys.length === 0 && !loadingMeta ? (
              <p className="mt-1 text-xs text-muted-foreground">
                {t("chat.noClientKeys")}{" "}
                <Link to="/client-keys" className="text-primary underline-offset-2 hover:underline">
                  {t("chat.manageKeys")}
                </Link>
              </p>
            ) : null}
            {metaError ? <p className="mt-2 text-xs text-destructive">{metaError}</p> : null}
            {loadingMeta || loadingSecret ? (
              <div className="mt-2 flex items-center gap-2 text-xs text-muted-foreground">
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
                {loadingSecret ? t("chat.loadingSecret") : t("chat.loadingMeta")}
              </div>
            ) : null}
          </div>
        </aside>

        <section className="flex min-h-0 min-w-0 flex-1 flex-col">
          <div className="flex items-center justify-between gap-3 border-b border-border/50 px-3 py-2.5 sm:px-4 sm:py-3">
            <div className="flex min-w-0 items-center gap-2">
              <Button type="button" size="sm" variant="ghost" className="rounded-full lg:hidden" onClick={() => setSidebarOpen(true)}>
                <Menu className="h-4 w-4" />
              </Button>
              <div className="min-w-0">
                <h1 className="truncate text-sm font-semibold tracking-tight sm:text-base md:text-lg">
                  {activeSession.title || t("chat.untitled")}
                </h1>
                <p className="truncate text-[11px] text-muted-foreground sm:text-xs">
                  {activeSession.mode === "image" ? "对话内生图 · 结果直接显示在会话中" : t("chat.subtitle")}
                </p>
              </div>
            </div>
            <div className="hidden items-center gap-2 sm:flex">
              <span
                className={`rounded-full px-2.5 py-1 text-[11px] ${
                  activeSession.mode === "image"
                    ? "bg-cyan-500/15 text-cyan-700 dark:text-cyan-300"
                    : "bg-muted text-muted-foreground"
                }`}
              >
                {activeSession.mode === "image" ? "生图模式" : "对话模式"}
              </span>
              <span className="max-w-[10rem] truncate text-xs text-muted-foreground md:max-w-[16rem]">
                {displayModelName(
                  activeSession.mode === "image"
                    ? activeModel || imageModels[0] || "grok-imagine-image"
                    : activeModel || chatModels[0] || ""
                )}
              </span>
            </div>
          </div>

          <div ref={threadRef} className="min-h-0 flex-1 overflow-y-auto px-3 py-3 sm:px-4 sm:py-4">
            <MessageList
              messages={activeSession.messages}
              emptyHint={
                activeSession.mode === "image"
                  ? "输入画面描述后点击生成，结果会显示在这里。"
                  : t("chat.threadEmpty")
              }
              onFillPrompt={fillPrompt}
              onResend={(prompt, references) => {
                void send(prompt, undefined, references);
              }}
              onRegenerate={(prompt) => {
                const nextPrompt = prompt.trim();
                if (!nextPrompt) return;
                if (activeSession.mode !== "image") {
                  updateSession(activeSession.id, (session) => ({
                    ...session,
                    mode: "image",
                    model: pickImageModel(imageModels, session.imageSettings.quality),
                    updatedAt: Date.now(),
                  }));
                }
                void send(nextPrompt, "image");
              }}
              onDeleteMessage={requestDeleteMessage}
              onOpenImageSettings={openImageSettings}
              onStopTask={stopTask}
            />
          </div>

          <div className="border-t border-border/50 px-3 py-2.5 sm:px-4 sm:py-3">
            <Composer
              key={`${activeSession.id}:${activeSession.mode}`}
              mode={activeSession.mode}
              value={draft}
              sending={chatSending}
              disabled={!effectiveClientKeyId || !clientSecret || loadingSecret}
              chatModels={chatModels}
              imageModels={imageModels}
              model={activeModel}
              imageSettings={activeImageSettings}
              attachments={draftImages}
              onChange={setDraft}
              onSend={() => void send()}
              onStop={() => {
                if (activeChatMessage) stopTask(activeChatMessage.id);
              }}
              onModeChange={(mode: ChatMode) => {
                updateSession(activeSession.id, (session) => ({
                  ...session,
                  mode,
                  model:
                    mode === "image"
                      ? pickImageModel(imageModels, session.imageSettings.quality)
                      : session.model && chatModels.includes(session.model)
                        ? session.model
                        : chatModels[0] || "",
                  updatedAt: Date.now(),
                }));
              }}
              onModelChange={(model) =>
                updateSession(activeSession.id, (session) => ({
                  ...session,
                  model,
                  updatedAt: Date.now(),
                }))
              }
              onImageSettingsChange={(imageSettings: ImageSettings) => {
                const normalized = imageSettingsForAvailableModels(imageSettings, imageModels);
                updateSession(activeSession.id, (session) => ({
                  ...session,
                  imageSettings: normalized,
                  model: session.model && imageModels.includes(session.model)
                    ? session.model
                    : pickImageModel(imageModels, normalized.quality),
                  updatedAt: Date.now(),
                }));
              }}
              onAttachmentsChange={setDraftImages}
            />
          </div>
        </section>
      </div>
    </div>

      <AlertDialog
        open={Boolean(pendingDelete)}
        onOpenChange={(open) => {
          if (!open) setPendingDelete(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {pendingDelete?.type === "session" ? "删除会话？" : "删除消息？"}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {pendingDelete?.type === "session"
                ? "删除后不可恢复，该会话中的消息也会一并清除。"
                : "删除后不可恢复。"}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90 focus-visible:ring-destructive/30"
              onClick={confirmPendingDelete}
            >
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>

  );
}
