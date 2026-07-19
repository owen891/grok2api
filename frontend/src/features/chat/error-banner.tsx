import { AlertCircle, Box, CircleDollarSign, KeyRound, Network, ShieldAlert, Timer, UserRoundX, WifiOff } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import type { ChatErrorClass } from "./chat-types";

const iconFor: Record<ChatErrorClass, typeof AlertCircle> = {
  auth: KeyRound,
  account: UserRoundX,
  model: Box,
  quota: CircleDollarSign,
  moderation: ShieldAlert,
  egress: Network,
  upstream: ShieldAlert,
  timeout: Timer,
  rate: WifiOff,
  unknown: AlertCircle,
};

const titleKeyFor: Record<ChatErrorClass, string> = {
  auth: "chat.error.auth",
  account: "chat.error.account",
  model: "chat.error.model",
  quota: "chat.error.quota",
  moderation: "chat.error.moderation",
  egress: "chat.error.egress",
  upstream: "chat.error.upstream",
  timeout: "chat.error.timeout",
  rate: "chat.error.rate",
  unknown: "chat.error.unknown",
};

export function ChatErrorBanner({
  errorClass,
  message,
  code,
  requestId,
}: {
  errorClass: ChatErrorClass;
  message: string;
  code?: string;
  requestId?: string;
}) {
  const { t } = useTranslation();
  const Icon = iconFor[errorClass] ?? AlertCircle;
  const title = t(titleKeyFor[errorClass] ?? titleKeyFor.unknown);

  return (
    <div className="flex items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
      <Icon className="mt-0.5 h-4 w-4 shrink-0" />
      <div className="min-w-0 space-y-0.5">
        <div className="font-medium">{title}</div>
        <div className="break-words text-destructive/90">{message}</div>
        {code ? <div className="text-xs opacity-70">code: {code}</div> : null}
        {requestId ? (
          <Link
            to={`/request-audits?search=${encodeURIComponent(requestId)}`}
            className="inline-block text-xs underline underline-offset-2 opacity-80 hover:opacity-100"
          >
            {t("chat.error.viewAudit", { requestId })}
          </Link>
        ) : null}
      </div>
    </div>
  );
}
