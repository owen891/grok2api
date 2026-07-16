import type { ReactNode } from "react";
import { Image as ImageIcon, MessageSquareText } from "lucide-react";
import type { ChatMode, ImageSettings } from "./chat-types";
import { ASPECT_RATIO_OPTIONS, RESOLUTION_OPTIONS } from "./chat-types";

function FieldLabel({ children }: { children: ReactNode }) {
  return <label className="mb-1 block text-[11px] font-medium text-muted-foreground">{children}</label>;
}

function NativeSelect({
  value,
  onChange,
  disabled,
  children,
}: {
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  children: ReactNode;
}) {
  return (
    <select
      value={value}
      disabled={disabled}
      onChange={(event) => onChange(event.target.value)}
      className="h-9 w-full rounded-lg border border-border/60 bg-background px-2.5 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60"
    >
      {children}
    </select>
  );
}

const FALLBACK_IMAGE_MODELS = ["grok-imagine-image"] as const;

export function ChatControls({
  mode,
  model,
  chatModels,
  imageModels,
  imageSettings,
  disabled,
  onModeChange,
  onModelChange,
  onImageSettingsChange,
}: {
  mode: ChatMode;
  model: string;
  chatModels: string[];
  imageModels: string[];
  imageSettings: ImageSettings;
  disabled?: boolean;
  onModeChange: (mode: ChatMode) => void;
  onModelChange: (model: string) => void;
  onImageSettingsChange: (settings: ImageSettings) => void;
}) {
  const isImage = mode === "image";
  const modelsForImage = imageModels.length > 0 ? imageModels : [...FALLBACK_IMAGE_MODELS];
  const imageModelValue = modelsForImage.includes(model) ? model : modelsForImage[0];
  const fastImage = !/quality/i.test(imageModelValue);
  const chatModelValue = chatModels.includes(model) ? model : chatModels[0] || "";

  return (
    <div
      className={`space-y-3 rounded-2xl border p-3 ${
        isImage ? "border-cyan-500/40 bg-cyan-500/5" : "border-border/60 bg-card/70"
      }`}
      data-mode={mode}
    >
      <div className="flex flex-wrap items-center gap-2">
        <div className="inline-flex rounded-full border border-border/60 bg-background p-1">
          <button
            type="button"
            disabled={disabled}
            onClick={() => onModeChange("chat")}
            className={`inline-flex h-8 items-center gap-1.5 rounded-full px-3 text-sm transition ${
              !isImage ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:bg-muted"
            }`}
          >
            <MessageSquareText className="h-3.5 w-3.5" />
            对话
          </button>
          <button
            type="button"
            disabled={disabled}
            onClick={() => onModeChange("image")}
            className={`inline-flex h-8 items-center gap-1.5 rounded-full px-3 text-sm transition ${
              isImage ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:bg-muted"
            }`}
          >
            <ImageIcon className="h-3.5 w-3.5" />
            生图
          </button>
        </div>

        <div className="min-w-[14rem] flex-1">
          <FieldLabel>{isImage ? "生图模型" : "对话模型"}</FieldLabel>
          {isImage ? (
            <NativeSelect
              value={imageModelValue}
              disabled={disabled}
              onChange={(value) => {
                onModelChange(value);
                onImageSettingsChange({
                  ...imageSettings,
                  quality: /quality/i.test(value) ? "quality" : "speed",
                  aspectRatio: /quality/i.test(value) ? imageSettings.aspectRatio : "1:1",
                  resolution: /quality/i.test(value) ? imageSettings.resolution : "1k",
                });
              }}
            >
              {modelsForImage.map((id) => (
                <option key={id} value={id}>
                  {id}
                </option>
              ))}
            </NativeSelect>
          ) : (
            <NativeSelect value={chatModelValue} disabled={disabled || chatModels.length === 0} onChange={onModelChange}>
              {chatModels.length === 0 ? <option value="">暂无可用对话模型</option> : null}
              {chatModels.map((id) => (
                <option key={id} value={id}>
                  {id}
                </option>
              ))}
            </NativeSelect>
          )}
        </div>
      </div>

      {isImage ? (
        <div className="grid gap-2 sm:grid-cols-3">
          <div>
            <FieldLabel>数量</FieldLabel>
            <NativeSelect
              value={String(imageSettings.n)}
              disabled={disabled}
              onChange={(value) => onImageSettingsChange({ ...imageSettings, n: Number(value) })}
            >
              {[1, 2, 3, 4].map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </NativeSelect>
          </div>
          <div>
            <FieldLabel>宽高比</FieldLabel>
            <NativeSelect
              value={fastImage ? "1:1" : imageSettings.aspectRatio}
              disabled={disabled || fastImage}
              onChange={(value) => onImageSettingsChange({ ...imageSettings, aspectRatio: value })}
            >
              {(fastImage ? ["1:1"] : ASPECT_RATIO_OPTIONS).map((ratio) => (
                <option key={ratio} value={ratio}>
                  {ratio}
                </option>
              ))}
            </NativeSelect>
          </div>
          <div>
            <FieldLabel>分辨率</FieldLabel>
            <NativeSelect
              value={fastImage ? "1k" : imageSettings.resolution}
              disabled={disabled || fastImage}
              onChange={(value) => onImageSettingsChange({ ...imageSettings, resolution: value })}
            >
              {(fastImage ? ["1k"] : RESOLUTION_OPTIONS).map((resolution) => (
                <option key={resolution} value={resolution}>
                  {resolution}
                </option>
              ))}
            </NativeSelect>
          </div>
        </div>
      ) : null}
    </div>
  );
}
