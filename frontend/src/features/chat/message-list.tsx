import { useEffect, useState, type ReactNode } from "react";
import {
  Bot,
  Copy,
  Download,
  ExternalLink,
  Images,
  Pencil,
  RefreshCw,
  Settings2,
  Square,
  Trash2,
  User,
} from "lucide-react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import type { ChatImageRef, ChatMessage } from "./chat-types";
import { formatGenerationMeta } from "./chat-types";
import { ChatErrorBanner } from "./error-banner";
import { ImageLightbox, type LightboxImage } from "./image-lightbox";

function plainTextBlocks(content: string) {
  return content.split(/\n{2,}/).map((block, index) => (
    <p key={index} className="whitespace-pre-wrap break-words leading-relaxed">
      {block}
    </p>
  ));
}

function guessFilename(url: string, index: number): string {
  try {
    const path = new URL(url, window.location.origin).pathname;
    const base = path.split("/").filter(Boolean).pop();
    if (base && base.includes(".")) return base;
    if (base) return `${base}.jpg`;
  } catch {
    // Ignore malformed URLs and use a stable fallback name.
  }
  return `grok-image-${index + 1}.jpg`;
}

async function downloadImage(url: string, filename: string): Promise<void> {
  try {
    const response = await fetch(url);
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    const blob = await response.blob();
    const objectUrl = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = objectUrl;
    anchor.download = filename;
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
    URL.revokeObjectURL(objectUrl);
  } catch {
    window.open(url, "_blank", "noopener,noreferrer");
  }
}

async function copyText(text: string, success = "已复制"): Promise<void> {
  try {
    await navigator.clipboard.writeText(text);
    toast.success(success);
  } catch {
    toast.error("复制失败");
  }
}

function ActionButton({
  label,
  onClick,
  danger,
  children,
}: {
  label: string;
  onClick: () => void;
  danger?: boolean;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      title={label}
      aria-label={label}
      onClick={onClick}
      className={`inline-flex h-7 w-7 items-center justify-center rounded-md border border-border/60 bg-background/80 text-muted-foreground transition hover:bg-muted hover:text-foreground ${
        danger ? "hover:border-destructive/40 hover:bg-destructive/10 hover:text-destructive" : ""
      }`}
    >
      {children}
    </button>
  );
}

function useElapsedSeconds(active: boolean, startedAt?: number) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (!active) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [active]);

  if (!active) return 0;
  const start = startedAt && startedAt > 0 ? startedAt : now;
  return Math.max(0, Math.floor((now - start) / 1000));
}

function StreamingSkeleton({
  startedAt,
  kind,
  progress,
}: {
  startedAt: number;
  kind: "chat" | "image";
  progress?: number;
}) {
  const seconds = useElapsedSeconds(true, startedAt);
  return (
    <div className="space-y-2 py-1">
      <div className="h-3 w-28 animate-pulse rounded bg-muted" />
      {kind === "image" ? <div className="h-40 animate-pulse rounded-xl bg-muted/80" /> : null}
      <div className="text-xs text-muted-foreground">
        {kind === "image" ? "图片任务处理中" : "正在生成回复"}，已等待 {seconds} 秒
        {typeof progress === "number" && progress > 0 ? ` · ${progress}%` : ""}
      </div>
    </div>
  );
}

export function MessageList({
  messages,
  emptyHint,
  onFillPrompt,
  onResend,
  onRegenerate,
  onDeleteMessage,
  onOpenImageSettings,
  onStopTask,
}: {
  messages: ChatMessage[];
  emptyHint: string;
  onFillPrompt?: (prompt: string) => void;
  onResend?: (prompt: string, references?: ChatImageRef[]) => void;
  onRegenerate?: (prompt: string, references?: ChatImageRef[]) => void;
  onDeleteMessage?: (messageId: string) => void;
  onOpenImageSettings?: () => void;
  onStopTask?: (messageId: string) => void;
}) {
  const [preview, setPreview] = useState<LightboxImage | null>(null);

  if (messages.length === 0) {
    return (
      <div className="flex h-full min-h-[280px] flex-col items-center justify-center gap-2 px-6 text-center">
        <div className="rounded-2xl border border-dashed border-border/70 bg-muted/20 px-8 py-10">
          <div className="text-base font-medium text-foreground/90">开始一段新对话</div>
          <p className="mt-2 max-w-md text-sm text-muted-foreground">{emptyHint}</p>
        </div>
      </div>
    );
  }

  return (
    <>
      <div className="space-y-5">
        {messages.map((message, messageIndex) => {
          const isUser = message.role === "user";
          const previousUserPrompt =
            !isUser && messageIndex > 0 && messages[messageIndex - 1]?.role === "user"
              ? messages[messageIndex - 1].content
              : "";
          const previousUserReferences =
            !isUser && messageIndex > 0 && messages[messageIndex - 1]?.role === "user"
              ? messages[messageIndex - 1].images
              : undefined;
          const canRetry = Boolean(
            onRegenerate && previousUserPrompt && message.task?.status !== "running" && (message.images?.length || message.error || message.content),
          );
          const taskRunning = message.task?.status === "queued" || message.task?.status === "running";
          const needsWideAssistant = Boolean(
            !isUser && (taskRunning || message.streaming || message.error),
          );
          const hasAssistantImages = Boolean(!isUser && message.images?.length);
          const hasMultipleAssistantImages = Boolean(!isUser && (message.images?.length ?? 0) > 1);

          return (
            <div
              key={message.id}
              data-message-role={message.role}
              className={`flex ${isUser ? "justify-end" : "justify-start"}`}
            >
              <div
                className={`flex min-w-0 flex-col ${isUser ? "items-end" : "items-start"} ${
                  isUser
                    ? "max-w-[88%] sm:max-w-[min(76%,46rem)]"
                    : needsWideAssistant
                      ? "w-full max-w-[min(94%,64rem)]"
                      : hasMultipleAssistantImages
                        ? "w-full max-w-[min(94%,64rem)]"
                      : hasAssistantImages
                        ? "max-w-[94%] sm:max-w-[min(90%,64rem)]"
                      : "max-w-[92%] sm:max-w-[min(82%,56rem)]"
                }`}
              >
                <div className={`mb-1.5 flex items-center gap-2 ${isUser ? "flex-row-reverse" : "flex-row"}`}>
                  <div
                    className={`inline-flex h-7 w-7 items-center justify-center rounded-full border border-border/60 ${
                      isUser ? "bg-secondary text-secondary-foreground" : "bg-background text-muted-foreground"
                    }`}
                    aria-hidden
                  >
                    {isUser ? <User className="h-3.5 w-3.5" /> : <Bot className="h-3.5 w-3.5" />}
                  </div>

                  <div className={`flex items-center gap-1 ${isUser ? "flex-row-reverse" : "flex-row"}`}>
                    {message.content ? (
                      <ActionButton
                        label="复制"
                        onClick={() => void copyText(message.content, "内容已复制")}
                      >
                        <Copy className="h-3.5 w-3.5" />
                      </ActionButton>
                    ) : null}

                    {isUser && message.content ? (
                      <>
                        <ActionButton
                          label="编辑"
                          onClick={() => onFillPrompt?.(message.content)}
                        >
                          <Pencil className="h-3.5 w-3.5" />
                        </ActionButton>
                        <ActionButton
                          label="重发"
                          onClick={() => {
                            onResend?.(message.content, message.images);
                          }}
                        >
                          <RefreshCw className="h-3.5 w-3.5" />
                        </ActionButton>
                      </>
                    ) : null}

                    {!isUser && canRetry ? (
                        <ActionButton
                          label="重新生成"
                          onClick={() => {
                            onRegenerate?.(previousUserPrompt, previousUserReferences);
                        }}
                      >
                        <RefreshCw className="h-3.5 w-3.5" />
                      </ActionButton>
                    ) : null}

                    {!isUser && taskRunning && onStopTask ? (
                      <ActionButton label="停止此任务" onClick={() => onStopTask(message.id)}>
                        <Square className="h-3.5 w-3.5" />
                      </ActionButton>
                    ) : null}

                    {onDeleteMessage ? (
                      <ActionButton
                        label="删除"
                        danger
                        onClick={() => onDeleteMessage(message.id)}
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                      </ActionButton>
                    ) : null}
                  </div>
                </div>

                <div
                  data-message-bubble
                  className={`min-w-0 max-w-full rounded-2xl px-4 py-3 text-sm shadow-sm ${needsWideAssistant || hasMultipleAssistantImages ? "w-full" : ""} ${
                    isUser
                      ? "rounded-tr-md border border-border/50 bg-secondary/70 text-foreground"
                      : "rounded-tl-md border border-border/60 bg-card/90 text-card-foreground backdrop-blur"
                  }`}
                >
                  <div className="mb-1 text-[11px] font-medium tracking-wide text-muted-foreground">
                    {isUser ? "你" : "助手"}
                    {taskRunning || message.streaming ? " · 生成中" : ""}
                  </div>

                  {(taskRunning || message.streaming) && !message.content && !(message.images && message.images.length) ? (
                    <StreamingSkeleton
                      startedAt={message.createdAt}
                      kind={message.task?.kind || "chat"}
                      progress={message.task?.progress}
                    />
                  ) : null}

                  {message.content ? <div className="space-y-2">{plainTextBlocks(message.content)}</div> : null}

                  {message.images && message.images.length > 0 ? (
                    <div className="mt-3 space-y-2">
                      {isUser ? (
                        <div className="flex flex-wrap gap-2">
                          {message.images.map((image, index) => (
                            <button
                              key={`${message.id}_ref_${index}`}
                              type="button"
                              className="group relative size-20 overflow-hidden rounded-lg border border-border/60 bg-muted"
                              onClick={() => setPreview({ url: image.url, name: `参考图-${index + 1}`, index })}
                              title={`参考图 ${index + 1}`}
                            >
                              <img src={image.previewUrl || image.url} alt={`参考图 ${index + 1}`} className="size-full object-cover" loading="lazy" />
                              <span className="pointer-events-none absolute inset-x-0 bottom-0 bg-black/60 px-1 py-0.5 text-[9px] text-white opacity-0 transition group-hover:opacity-100">参考图</span>
                            </button>
                          ))}
                        </div>
                      ) : null}
                      {!isUser ? <div className={`grid max-w-full justify-items-start gap-2 ${message.images.length > 1 ? "w-full grid-cols-1 sm:grid-cols-2" : "w-fit grid-cols-1"}`}>
                        {message.images.map((image, index) => {
                          const filename = guessFilename(image.url, index);
                          return (
                            <div
                              key={`${message.id}_img_${index}`}
                              className="w-fit max-w-[min(58vw,16rem)] overflow-hidden rounded-xl border border-border/50 bg-background/40"
                            >
                              <button
                                type="button"
                                className="block w-fit max-w-full cursor-zoom-in text-left"
                                onClick={() =>
                                  setPreview({
                                    url: image.url,
                                    name: filename,
                                    index,
                                    items: message.images?.map((item, itemIndex) => ({
                                      url: item.url,
                                      name: guessFilename(item.url, itemIndex),
                                      index: itemIndex,
                                    })),
                                  })
                                }
                                aria-label={`预览图片 ${index + 1}`}
                              >
                                <img
                                  src={image.url}
                                  alt={`生成图片 ${index + 1}`}
                                  className="block h-auto w-auto max-h-[min(50vh,28rem)] max-w-full object-contain transition hover:opacity-95"
                                  loading="lazy"
                                />
                              </button>
                              <div className="flex flex-nowrap items-center justify-center gap-1.5 border-t border-border/40 bg-background/60 px-2 py-1.5">
                                <button
                                  type="button"
                                  onClick={() =>
                                    setPreview({
                                      url: image.url,
                                      name: filename,
                                      index,
                                      items: message.images?.map((item, itemIndex) => ({
                                        url: item.url,
                                        name: guessFilename(item.url, itemIndex),
                                        index: itemIndex,
                                      })),
                                    })
                                  }
                                  className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground transition hover:bg-muted hover:text-foreground"
                                  title="打开图片"
                                  aria-label="打开图片"
                                >
                                  <ExternalLink className="h-3.5 w-3.5" />
                                </button>
                                <button
                                  type="button"
                                  onClick={() => void downloadImage(image.url, filename)}
                                  className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground transition hover:bg-muted hover:text-foreground"
                                  title="下载图片"
                                  aria-label="下载图片"
                                >
                                  <Download className="h-3.5 w-3.5" />
                                </button>
                                <button
                                  type="button"
                                  onClick={() => void copyText(image.url)}
                                  className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground transition hover:bg-muted hover:text-foreground"
                                  title="复制图片链接"
                                  aria-label="复制图片链接"
                                >
                                  <Copy className="h-3.5 w-3.5" />
                                </button>
                                <Link
                                  to="/gallery"
                                  className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground transition hover:bg-muted hover:text-foreground"
                                  title="打开图库"
                                  aria-label="打开图库"
                                >
                                  <Images className="h-3.5 w-3.5" />
                                </Link>
                              </div>
                            </div>
                          );
                        })}
                      </div> : null}
                    </div>
                  ) : null}

                  {message.error ? (
                    <div className="mt-3 space-y-2">
                      <ChatErrorBanner
                        errorClass={message.error.class}
                        message={message.error.message}
                        code={message.error.code}
                        requestId={message.error.requestId}
                      />
                      {previousUserPrompt ? (
                        <div className="flex flex-wrap gap-2">
                          <button
                            type="button"
                            onClick={() => onRegenerate?.(previousUserPrompt, previousUserReferences)}
                            className="inline-flex items-center gap-1 rounded-md border border-border/60 bg-background px-2.5 py-1.5 text-[11px] text-muted-foreground hover:bg-muted hover:text-foreground disabled:opacity-50"
                          >
                            <RefreshCw className="h-3 w-3" />
                            相同参数重试
                          </button>
                          <button
                            type="button"
                            onClick={() => {
                              onFillPrompt?.(previousUserPrompt);
                              onOpenImageSettings?.();
                              toast.message("已填入提示词，可调整参数后重新生成");
                            }}
                            className="inline-flex items-center gap-1 rounded-md border border-border/60 bg-background px-2.5 py-1.5 text-[11px] text-muted-foreground hover:bg-muted hover:text-foreground disabled:opacity-50"
                          >
                            <Settings2 className="h-3 w-3" />
                            改参数再试
                          </button>
                        </div>
                      ) : null}
                    </div>
                  ) : null}

                  {!isUser && message.generation ? (
                    <div className="mt-2 text-[11px] text-muted-foreground">
                      {formatGenerationMeta(message.generation)}
                    </div>
                  ) : null}

                  {!isUser && message.task?.kind === "image" && message.task.requestId ? (
                    <div className="mt-1 break-all text-[10px] text-muted-foreground/75">
                      任务 ID · {message.task.requestId}
                    </div>
                  ) : null}
                </div>
              </div>
            </div>
          );
        })}
      </div>

      <ImageLightbox image={preview} onClose={() => setPreview(null)} />
    </>
  );
}
