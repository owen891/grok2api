import { useEffect, useMemo, useState } from "react";
import { ChevronLeft, ChevronRight, Copy, Download, ExternalLink, X } from "lucide-react";
import { createPortal } from "react-dom";
import { toast } from "sonner";

import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import type { MediaJobDTO } from "@/features/media/types";
import { formatDateTime } from "@/shared/lib/format";

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

async function copyText(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text);
    toast.success("链接已复制");
  } catch {
    toast.error("复制失败");
  }
}

function guessFilename(url: string, index = 0): string {
  try {
    const path = new URL(url, window.location.origin).pathname;
    const base = path.split("/").filter(Boolean).pop();
    if (base && base.includes(".")) return base;
    if (base) return `${base}.jpg`;
  } catch {
    // ignore
  }
  return `grok-image-${index + 1}.jpg`;
}

export type LightboxImage = {
  url: string;
  name?: string;
  index?: number;
  items?: Array<{ url: string; name?: string; index?: number }>;
};

export function ImageLightbox({
  image,
  onClose,
}: {
  image: LightboxImage | null;
  onClose: () => void;
}) {
  const items = useMemo(() => {
    if (!image) return [];
    if (image.items && image.items.length > 0) return image.items;
    return [{ url: image.url, name: image.name, index: image.index ?? 0 }];
  }, [image]);

  const initialIndex = useMemo(() => {
    if (!image) return 0;
    if (typeof image.index === "number") {
      const byIndex = items.findIndex((item) => item.index === image.index);
      if (byIndex >= 0) return byIndex;
    }
    const byUrl = items.findIndex((item) => item.url === image.url);
    return byUrl >= 0 ? byUrl : 0;
  }, [image, items]);

  const [activeIndex, setActiveIndex] = useState(initialIndex);

  useEffect(() => {
    if (!image) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
      if (event.key === "ArrowLeft") {
        setActiveIndex((prev) => (prev - 1 + items.length) % items.length);
      }
      if (event.key === "ArrowRight") {
        setActiveIndex((prev) => (prev + 1) % items.length);
      }
    };
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    window.addEventListener("keydown", onKeyDown);
    return () => {
      document.body.style.overflow = previousOverflow;
      window.removeEventListener("keydown", onKeyDown);
    };
  }, [image, items.length, onClose]);

  if (!image || typeof document === "undefined") return null;

  const current = items[Math.min(activeIndex, items.length - 1)] ?? items[0];
  const filename = current.name || guessFilename(current.url, current.index ?? activeIndex);
  const hasMultiple = items.length > 1;

  return createPortal(
    <div className="fixed inset-0 z-[80]" role="dialog" aria-modal="true" aria-label="图片预览">
      {/* 整层遮罩可点空白关闭 */}
      <button
        type="button"
        className="absolute inset-0 cursor-default bg-black/70 backdrop-blur-md"
        aria-label="关闭预览"
        onClick={onClose}
      />

      <div className="pointer-events-none relative flex h-full w-full items-center justify-center p-4 sm:p-6">
        <div className="relative flex max-h-[92vh] w-full max-w-[min(92vw,72rem)] flex-col items-center">
          <button
            type="button"
            onClick={onClose}
            className="pointer-events-auto absolute -top-11 right-0 inline-flex h-9 w-9 items-center justify-center rounded-full border border-white/30 bg-black/30 text-white transition hover:bg-white/15"
            aria-label="关闭"
          >
            <X className="h-4 w-4" />
          </button>

          {hasMultiple ? (
            <>
              <button
                type="button"
                onClick={() => setActiveIndex((prev) => (prev - 1 + items.length) % items.length)}
                className="pointer-events-auto absolute left-0 top-1/2 z-10 inline-flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full border border-white/30 bg-black/35 text-white transition hover:bg-white/15 sm:-left-12"
                aria-label="上一张"
              >
                <ChevronLeft className="h-5 w-5" />
              </button>
              <button
                type="button"
                onClick={() => setActiveIndex((prev) => (prev + 1) % items.length)}
                className="pointer-events-auto absolute right-0 top-1/2 z-10 inline-flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full border border-white/30 bg-black/35 text-white transition hover:bg-white/15 sm:-right-12"
                aria-label="下一张"
              >
                <ChevronRight className="h-5 w-5" />
              </button>
            </>
          ) : null}

          <img
            src={current.url}
            alt={filename}
            className="pointer-events-auto max-h-[78vh] max-w-full rounded-2xl object-contain shadow-2xl"
          />

          <div className="pointer-events-auto mt-3 flex max-w-full flex-wrap items-center justify-center gap-2 text-xs text-white/85">
            <span className="max-w-[16rem] truncate sm:max-w-[24rem]" title={filename}>
              {hasMultiple ? `${activeIndex + 1}/${items.length} · ${filename}` : filename}
            </span>
            <button
              type="button"
              onClick={() => void downloadImage(current.url, filename)}
              className="inline-flex items-center gap-1 rounded-full border border-white/35 px-3 py-1.5 text-[11px] text-white transition hover:border-white/65 hover:bg-white/10"
            >
              <Download className="h-3.5 w-3.5" />
              下载
            </button>
            <button
              type="button"
              onClick={() => void copyText(current.url)}
              className="inline-flex items-center gap-1 rounded-full border border-white/35 px-3 py-1.5 text-[11px] text-white transition hover:border-white/65 hover:bg-white/10"
            >
              <Copy className="h-3.5 w-3.5" />
              复制链接
            </button>
            <a
              href={current.url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 rounded-full border border-white/35 px-3 py-1.5 text-[11px] text-white transition hover:border-white/65 hover:bg-white/10"
            >
              <ExternalLink className="h-3.5 w-3.5" />
              新标签打开
            </a>
          </div>
        </div>
      </div>
    </div>,
    document.body,
  );
}

export function VideoJobDialog({
  job,
  open,
  onOpenChange,
  locale,
}: {
  job: MediaJobDTO | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  locale: string;
}) {
  const [copied, setCopied] = useState(false);

  if (!job) return null;

  const details = [
    ["任务 ID", job.id],
    ["状态", job.status],
    ["模型", job.model || "—"],
    ["进度", `${job.progress}%`],
    ["尺寸", job.size || "—"],
    ["画质", job.quality || "—"],
    ["秒数", String(job.seconds || "—")],
    ["账号", job.accountName || "—"],
    ["客户端密钥", job.clientKeyName || "—"],
    ["创建时间", formatDateTime(job.createdAt, locale)],
    ["完成时间", job.completedAt ? formatDateTime(job.completedAt, locale) : "—"],
  ] as const;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>视频任务详情</DialogTitle>
          <DialogDescription>
            查看任务状态与参数。当前接口未返回可播放视频地址，暂不支持内嵌播放。
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3 text-sm">
          <div className="rounded-xl border border-border/60 bg-muted/20 p-3">
            <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">提示词</div>
            <p className="mt-1 whitespace-pre-wrap break-words text-sm leading-relaxed text-foreground">
              {job.prompt || "（无提示词）"}
            </p>
          </div>

          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
            {details.map(([label, value]) => (
              <div key={label} className="rounded-lg border border-border/50 bg-background/70 px-3 py-2">
                <div className="text-[11px] text-muted-foreground">{label}</div>
                <div className="mt-0.5 break-all text-xs font-medium text-foreground">{value}</div>
              </div>
            ))}
          </div>

          {job.errorMessage ? (
            <div className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
              {job.errorMessage}
            </div>
          ) : null}

          <div className="flex flex-wrap gap-2">
            <button
              type="button"
              onClick={async () => {
                await copyText(job.id);
                setCopied(true);
              }}
              className="inline-flex items-center gap-1 rounded-lg border border-border/60 px-3 py-1.5 text-xs hover:bg-muted"
            >
              <Copy className="h-3.5 w-3.5" />
              {copied ? "已复制任务 ID" : "复制任务 ID"}
            </button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}
