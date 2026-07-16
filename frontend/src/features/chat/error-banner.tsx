import { AlertCircle, KeyRound, ShieldAlert, Timer, WifiOff } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { ChatErrorClass } from "./chat-types";

const iconFor: Record<ChatErrorClass, typeof AlertCircle> = {
  auth: KeyRound,
  upstream: ShieldAlert,
  timeout: Timer,
  rate: WifiOff,
  unknown: AlertCircle,
};

export function ChatErrorBanner({
  errorClass,
  message,
  code,
}: {
  errorClass: ChatErrorClass;
  message: string;
  code?: string;
}) {
  const { t } = useTranslation();
  const Icon = iconFor[errorClass] ?? AlertCircle;
  const title =
    errorClass === "auth"
      ? t("chat.error.auth")
      : errorClass === "upstream"
        ? t("chat.error.upstream")
        : errorClass === "timeout"
          ? t("chat.error.timeout")
          : errorClass === "rate"
            ? t("chat.error.rate")
            : t("chat.error.unknown");

  return (
    <div className="flex items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
      <Icon className="mt-0.5 h-4 w-4 shrink-0" />
      <div className="min-w-0 space-y-0.5">
        <div className="font-medium">{title}</div>
        <div className="break-words text-destructive/90">{message}</div>
        {code ? <div className="text-xs opacity-70">code: {code}</div> : null}
      </div>
    </div>
  );
}
