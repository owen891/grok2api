import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Image as ImageIcon,
  ImagePlus,
  Loader2,
  MessageSquareText,
  SendHorizontal,
  Settings2,
  Square,
  X,
} from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import type { ChatImageRef, ChatMode, ImageSettings } from "./chat-types";
import {
  ASPECT_RATIO_OPTIONS,
  QUALITY_IMAGE_MODEL,
  RESOLUTION_OPTIONS,
  SPEED_IMAGE_MODEL,
  displayModelName,
  imageSettingsForAvailableModels,
  imageSettingsForModel,
  normalizeImageSettings,
} from "./chat-types";

const FALLBACK_IMAGE_MODELS = [SPEED_IMAGE_MODEL];
export const OPEN_IMAGE_SETTINGS_EVENT = "studio:open-image-settings";
const MAX_CHAT_ATTACHMENTS = 4;
const MAX_CHAT_ATTACHMENT_BYTES = 5 * 1024 * 1024;

function readImageFile(file: File): Promise<ChatImageRef> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const url = typeof reader.result === "string" ? reader.result : "";
      if (!url) reject(new Error("图片读取失败"));
      else resolve({ url, previewUrl: url, mimeType: file.type || undefined });
    };
    reader.onerror = () => reject(reader.error || new Error("图片读取失败"));
    reader.readAsDataURL(file);
  });
}

type ImagePreset = {
  id: string;
  label: string;
  description: string;
  settings: Pick<ImageSettings, "n" | "aspectRatio" | "resolution" | "quality">;
};

const IMAGE_PRESETS: ImagePreset[] = [
  {
    id: "avatar",
    label: "头像",
    description: "1:1 · 1K · 1张",
    settings: { n: 1, aspectRatio: "1:1", resolution: "1k", quality: "speed" },
  },
  {
    id: "wallpaper",
    label: "壁纸",
    description: "16:9 · 2K · 1张",
    settings: { n: 1, aspectRatio: "16:9", resolution: "2k", quality: "quality" },
  },
  {
    id: "poster",
    label: "海报",
    description: "2:3 · 2K · 1张",
    settings: { n: 1, aspectRatio: "2:3", resolution: "2k", quality: "quality" },
  },
  {
    id: "story",
    label: "竖屏",
    description: "9:16 · 1K · 1张",
    settings: { n: 1, aspectRatio: "9:16", resolution: "1k", quality: "quality" },
  },
  {
    id: "batch",
    label: "多图对比",
    description: "1:1 · 1K · 4张",
    settings: { n: 4, aspectRatio: "1:1", resolution: "1k", quality: "speed" },
  },
];

function FieldSelect({
  label,
  value,
  onChange,
  disabled,
  children,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  children: ReactNode;
}) {
  return (
    <label className="block space-y-1.5">
      <span className="text-xs font-medium text-muted-foreground">{label}</span>
      <select
        value={value}
        disabled={disabled}
        onChange={(event) => onChange(event.target.value)}
        className="h-9 w-full rounded-lg border border-border/60 bg-background px-2.5 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60"
      >
        {children}
      </select>
    </label>
  );
}

function matchesPreset(settings: ImageSettings, preset: ImagePreset): boolean {
  return (
    settings.n === preset.settings.n &&
    settings.aspectRatio === preset.settings.aspectRatio &&
    settings.resolution === preset.settings.resolution &&
    settings.quality === preset.settings.quality
  );
}

export function Composer({
  mode,
  value,
  disabled,
  sending,
  chatModels,
  imageModels,
  model,
  imageSettings,
  attachments,
  onChange,
  onSend,
  onStop,
  onModeChange,
  onModelChange,
  onImageSettingsChange,
  onAttachmentsChange,
}: {
  mode: ChatMode;
  value: string;
  disabled?: boolean;
  sending?: boolean;
  chatModels: string[];
  imageModels: string[];
  model: string;
  imageSettings: ImageSettings;
  attachments: ChatImageRef[];
  onChange: (value: string) => void;
  onSend: () => void;
  onStop: () => void;
  onModeChange: (mode: ChatMode) => void;
  onModelChange: (model: string) => void;
  onImageSettingsChange: (settings: ImageSettings) => void;
  onAttachmentsChange: (attachments: ChatImageRef[]) => void;
}) {
  const [dialogOpen, setDialogOpen] = useState(false);
  const [draftSettings, setDraftSettings] = useState<ImageSettings>(imageSettings);
  const [draftModel, setDraftModel] = useState(model);
  const fileInputRef = useRef<HTMLInputElement | null>(null);

  const isImage = mode === "image";
  const modelsForImage = useMemo(
    () => {
      const generationModels = imageModels.filter((id) => !/edit/i.test(id));
      return generationModels.length > 0 ? generationModels : FALLBACK_IMAGE_MODELS;
    },
    [imageModels],
  );
  const imageModelValue = modelsForImage.includes(model) ? model : modelsForImage[0];
  const qualityModel = modelsForImage.find((id) => /quality/i.test(id));
  const chatModelValue = chatModels.includes(model) ? model : chatModels[0] || "";
  const compatibleImageSettings = imageSettingsForAvailableModels(imageSettings, modelsForImage);
  const summary = `${compatibleImageSettings.n}张 · ${compatibleImageSettings.aspectRatio} · ${compatibleImageSettings.resolution}`;
  const activePresetId = IMAGE_PRESETS.find((preset) => matchesPreset(compatibleImageSettings, preset))?.id;

  const openSettings = useCallback(() => {
    setDraftSettings(compatibleImageSettings);
    setDraftModel(imageModelValue);
    setDialogOpen(true);
  }, [compatibleImageSettings, imageModelValue]);

  const addImageFiles = useCallback((files: File[]) => {
    const remaining = MAX_CHAT_ATTACHMENTS - attachments.length;
    if (remaining <= 0) {
      toast.error(`最多添加 ${MAX_CHAT_ATTACHMENTS} 张图片`);
      return;
    }
    const accepted = files.filter((file) => {
      if (!file.type.startsWith("image/")) return false;
      if (file.size > MAX_CHAT_ATTACHMENT_BYTES) {
        toast.error(`${file.name || "剪贴板图片"} 超过 5 MB`);
        return false;
      }
      return true;
    }).slice(0, remaining);
    if (accepted.length === 0) return;
    void Promise.all(accepted.map(readImageFile))
      .then((images) => onAttachmentsChange([...attachments, ...images]))
      .catch((error) => toast.error(error instanceof Error ? error.message : "图片读取失败"));
  }, [attachments, onAttachmentsChange]);

  useEffect(() => {
    const onOpen = () => {
      if (mode !== "image") return;
      openSettings();
    };
    window.addEventListener(OPEN_IMAGE_SETTINGS_EVENT, onOpen);
    return () => window.removeEventListener(OPEN_IMAGE_SETTINGS_EVENT, onOpen);
  }, [mode, openSettings]);

  const applySettings = () => {
    const nextSettings = imageSettingsForModel(normalizeImageSettings(draftSettings), draftModel);
    onModelChange(draftModel);
    onImageSettingsChange(nextSettings);
    setDialogOpen(false);
  };

  const applyPreset = (preset: ImagePreset, immediate = false) => {
    if (preset.settings.quality === "quality" && !qualityModel) return;
    const next: ImageSettings = {
      ...draftSettings,
      ...preset.settings,
    };

    // 高质量预设尽量切到 quality 模型；快速预设切回普通 imagine
    const nextModel =
      preset.settings.quality === "quality"
        ?
        qualityModel || draftModel
        :
        modelsForImage.find((id) => /imagine|image/i.test(id) && !/quality|edit/i.test(id)) ||
        modelsForImage.find((id) => !/edit/i.test(id)) ||
        FALLBACK_IMAGE_MODELS[0] ||
        draftModel;

    if (immediate) {
      onModelChange(nextModel);
      onImageSettingsChange(imageSettingsForModel(next, nextModel));
      return;
    }

    setDraftSettings(next);
    setDraftModel(nextModel);
  };

  return (
    <div className="relative rounded-2xl border border-border/60 bg-card/90 p-3 shadow-sm backdrop-blur">
      <textarea
        value={value}
        onChange={(event) => onChange(event.target.value)}
        onPaste={(event) => {
          if (isImage) return;
          const files = Array.from(event.clipboardData.items)
            .filter((item) => item.kind === "file" && item.type.startsWith("image/"))
            .map((item) => item.getAsFile())
            .filter((file): file is File => Boolean(file));
          if (files.length === 0) return;
          event.preventDefault();
          addImageFiles(files);
        }}
        onKeyDown={(event) => {
          if (event.key === "Enter" && !event.shiftKey) {
            event.preventDefault();
            if (!sending && !disabled && (value.trim() || (!isImage && attachments.length > 0))) onSend();
          }
        }}
        rows={3}
        disabled={disabled}
        placeholder={isImage ? "描述你想生成的图片…" : "输入消息，Enter 发送，Shift+Enter 换行"}
        className="w-full resize-none rounded-xl border border-border/50 bg-background px-3 py-2.5 text-sm outline-none placeholder:text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60"
      />

      {!isImage && attachments.length > 0 ? (
        <div className="mt-2 flex flex-wrap gap-2">
          {attachments.map((attachment, index) => (
            <div key={`${attachment.url.slice(0, 48)}_${index}`} className="group relative size-16 overflow-hidden rounded-lg border border-border/60 bg-muted">
              <img src={attachment.previewUrl || attachment.url} alt={`待发送图片 ${index + 1}`} className="size-full object-cover" />
              <button
                type="button"
                onClick={() => onAttachmentsChange(attachments.filter((_, itemIndex) => itemIndex !== index))}
                className="absolute right-1 top-1 inline-flex size-5 items-center justify-center rounded-full bg-black/65 text-white opacity-80 transition hover:opacity-100"
                title="移除图片"
                aria-label={`移除图片 ${index + 1}`}
              >
                <X className="h-3 w-3" />
              </button>
            </div>
          ))}
        </div>
      ) : null}

      <div className="mt-2 flex flex-wrap items-center gap-2">
        <div className="inline-flex shrink-0 rounded-full border border-border/60 bg-background p-0.5">
          <button
            type="button"
            onClick={() => onModeChange("chat")}
            className={`inline-flex h-8 items-center gap-1 rounded-full px-2.5 text-xs transition ${
              !isImage ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:bg-muted"
            }`}
          >
            <MessageSquareText className="h-3.5 w-3.5" />
            对话
          </button>
          <button
            type="button"
            onClick={() => onModeChange("image")}
            className={`inline-flex h-8 items-center gap-1 rounded-full px-2.5 text-xs transition ${
              isImage ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:bg-muted"
            }`}
          >
            <ImageIcon className="h-3.5 w-3.5" />
            生图
          </button>
        </div>

        {isImage ? (
          <>
            <select
              value={imageModelValue}
              disabled={modelsForImage.length === 0}
              onChange={(event) => {
                const nextModel = event.target.value;
                onModelChange(nextModel);
                onImageSettingsChange(imageSettingsForModel(compatibleImageSettings, nextModel));
              }}
              className="h-8 min-w-[9rem] max-w-full flex-1 rounded-lg border border-border/60 bg-background px-2 text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring sm:max-w-[16rem]"
              aria-label="生图模型"
            >
              {modelsForImage.map((id) => (
                <option key={id} value={id}>{`${displayModelName(id)}（${id}）`}</option>
              ))}
            </select>
            <button
              type="button"
              onClick={openSettings}
              className="inline-flex h-8 max-w-full items-center gap-1.5 rounded-lg border border-border/60 bg-background px-2.5 text-xs text-foreground hover:bg-muted"
              title={`生图参数：${summary}`}
            >
              <Settings2 className="h-3.5 w-3.5 shrink-0" />
              <span className="truncate">{summary}</span>
            </button>
          </>
        ) : (
          <select
            value={chatModelValue}
            disabled={chatModels.length === 0}
            onChange={(event) => onModelChange(event.target.value)}
            className="h-8 min-w-[9rem] max-w-full flex-1 rounded-lg border border-border/60 bg-background px-2 text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring sm:max-w-[16rem]"
          >
            {chatModels.length === 0 ? <option value="">暂无对话模型</option> : null}
            {chatModels.map((id) => (
              <option key={id} value={id}>
                {displayModelName(id)}
              </option>
            ))}
          </select>
        )}

        {isImage ? (
          <div className="inline-flex min-w-0 flex-wrap items-center gap-1.5">
            <span className="mr-0.5 text-[11px] text-muted-foreground">预设</span>
            {IMAGE_PRESETS.map((preset) => {
              const active = activePresetId === preset.id;
              const unavailable = preset.settings.quality === "quality" && !qualityModel;
              return (
                <button
                  key={preset.id}
                  type="button"
                  disabled={unavailable}
                  title={unavailable ? `${preset.description} · 需要可用的高质量生图模型` : preset.description}
                  onClick={() => applyPreset(preset, true)}
                  className={`inline-flex h-7 items-center rounded-full border px-2.5 text-[11px] transition ${
                    active
                      ? "border-primary/40 bg-primary/10 text-foreground"
                      : "border-border/60 bg-background text-muted-foreground hover:bg-muted hover:text-foreground disabled:cursor-not-allowed disabled:opacity-40"
                  }`}
                >
                  {preset.label}
                </button>
              );
            })}
          </div>
        ) : null}

        {!isImage ? (
          <>
            <input
              ref={fileInputRef}
              type="file"
              accept="image/*"
              multiple
              className="hidden"
              onChange={(event) => {
                const files = Array.from(event.target.files || []);
                event.target.value = "";
                addImageFiles(files);
              }}
            />
            <button
              type="button"
              onClick={() => fileInputRef.current?.click()}
              className="inline-flex size-8 shrink-0 items-center justify-center rounded-lg border border-border/60 bg-background text-muted-foreground transition hover:bg-muted hover:text-foreground"
              title="添加图片"
              aria-label="添加图片"
            >
              <ImagePlus className="h-3.5 w-3.5" />
            </button>
          </>
        ) : null}

        <div className="ml-auto flex shrink-0 items-center gap-2">
          {sending ? (
            <Button type="button" variant="outline" size="sm" className="rounded-full" onClick={onStop}>
              <Square className="mr-1.5 h-3.5 w-3.5" />
              停止
            </Button>
          ) : (
            <Button type="button" size="sm" className="rounded-full px-4" onClick={onSend} disabled={disabled || (!value.trim() && (isImage || attachments.length === 0))}>
              {disabled ? <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" /> : <SendHorizontal className="mr-1.5 h-3.5 w-3.5" />}
              {isImage ? "生成" : "发送"}
            </Button>
          )}
        </div>
      </div>

      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="max-w-lg">
          <DialogHeader>
            <DialogTitle>生图参数</DialogTitle>
            <DialogDescription>可先选预设，再微调模型、数量、比例和分辨率。</DialogDescription>
          </DialogHeader>

          <div className="grid gap-3 py-1">
            <div className="space-y-1.5">
              <div className="text-xs font-medium text-muted-foreground">常用预设</div>
              <div className="flex flex-wrap gap-1.5">
                {IMAGE_PRESETS.map((preset) => {
                  const active = matchesPreset(draftSettings, preset);
                  const unavailable = preset.settings.quality === "quality" && !qualityModel;
                  return (
                    <button
                      key={preset.id}
                      type="button"
                      disabled={unavailable}
                      title={unavailable ? "需要可用的高质量生图模型" : undefined}
                      onClick={() => applyPreset(preset, false)}
                      className={`rounded-lg border px-2.5 py-1.5 text-left transition ${
                        active
                          ? "border-primary/40 bg-primary/10"
                          : "border-border/60 bg-background hover:bg-muted disabled:cursor-not-allowed disabled:opacity-40"
                      }`}
                    >
                      <div className="text-xs font-medium text-foreground">{preset.label}</div>
                      <div className="text-[10px] text-muted-foreground">{preset.description}</div>
                    </button>
                  );
                })}
              </div>
            </div>

            <FieldSelect
              label="生图模型"
              value={modelsForImage.includes(draftModel) ? draftModel : modelsForImage[0]}
              onChange={(next) => {
                setDraftModel(next);
                setDraftSettings((prev) => imageSettingsForModel(prev, next));
              }}
            >
              {modelsForImage.map((id) => (
                <option key={id} value={id}>
                  {`${displayModelName(id)}（${id}）`}
                </option>
              ))}
            </FieldSelect>

            <div className="grid grid-cols-3 gap-2">
              <FieldSelect
                label="数量"
                value={String(draftSettings.n)}
                onChange={(next) => setDraftSettings((prev) => ({ ...prev, n: Number(next) }))}
              >
                {[1, 2, 3, 4].map((n) => (
                  <option key={n} value={n}>
                    {n} 张
                  </option>
                ))}
              </FieldSelect>

              <FieldSelect
                label="比例"
                value={/quality/i.test(draftModel) ? draftSettings.aspectRatio : "1:1"}
                disabled={!/quality/i.test(draftModel)}
                onChange={(next) => {
                  const settings = normalizeImageSettings({ ...draftSettings, aspectRatio: next });
                  setDraftSettings(settings);
                  if (settings.quality === "quality") {
                    setDraftModel(
                      qualityModel || QUALITY_IMAGE_MODEL,
                    );
                  }
                }}
              >
                {(/quality/i.test(draftModel) ? ASPECT_RATIO_OPTIONS : ["1:1"]).map((ratio) => (
                  <option key={ratio} value={ratio}>
                    {ratio}
                  </option>
                ))}
              </FieldSelect>

              <FieldSelect
                label="分辨率"
                value={/quality/i.test(draftModel) ? draftSettings.resolution : "1k"}
                disabled={!/quality/i.test(draftModel)}
                onChange={(next) => {
                  const settings = normalizeImageSettings({ ...draftSettings, resolution: next });
                  setDraftSettings(settings);
                  if (settings.quality === "quality") {
                    setDraftModel(
                      qualityModel || QUALITY_IMAGE_MODEL,
                    );
                  }
                }}
              >
                {(/quality/i.test(draftModel) ? RESOLUTION_OPTIONS : ["1k"]).map((resolution) => (
                  <option key={resolution} value={resolution}>
                    {resolution}
                  </option>
                ))}
              </FieldSelect>
            </div>
          </div>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setDialogOpen(false)}>
              取消
            </Button>
            <Button type="button" onClick={applySettings}>
              确认
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
