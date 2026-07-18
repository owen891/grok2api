import type { ChatErrorClass } from "./chat-types";

export type ClassifiedError = {
  class: ChatErrorClass;
  message: string;
  code?: string;
  status?: number;
  requestId?: string;
};

type OpenAIErrorBody = {
  error?: {
    message?: string;
    type?: string;
    code?: string | number;
    request_id?: string;
    requestId?: string;
  };
  message?: string;
  request_id?: string;
  requestId?: string;
};

function classifyErrorClass(status: number, code: string, message: string): ChatErrorClass {
  const signal = `${code} ${message}`.toLowerCase();
  if (/invalid_api_key|client[_ -]?key|authentication_error/.test(signal)) return "auth";
  if (/account_permission|permission[_ -]denied|credential_rejected|upstream_unauthorized/.test(signal)) return "account";
  if (/quota|usage[_ -]?limit|额度.*(?:不足|用完)|billing_limit|spending.limit|payment_required|insufficient.*balance/.test(signal)) return "quota";
  if (/model[_ -]not[_ -]found|model_not_allowed|model_unavailable|model_cooling|unsupported[_ -]model/.test(signal)) return "model";
  if (/egress_unavailable|browser_worker_unavailable|proxy|cloudflare|upstream_network_error/.test(signal)) return "egress";
  if (status === 408 || status === 504 || /timeout|timed_out/.test(signal)) return "timeout";
  if (status === 429 || /rate_limit/.test(signal)) return "rate";
  if (status === 401) return "auth";
  if (status === 402 || status === 403 || status >= 500 || code.startsWith("upstream_")) return "upstream";
  return "unknown";
}

export function localizeGatewayMessage(errorClass: ChatErrorClass, message: string, status?: number): string {
  const raw = (message || "").trim();
  switch (errorClass) {
    case "auth":
      return "客户端密钥无效、已过期或没有调用权限，请检查当前选择的密钥。";
    case "account":
      return "上游账号权限或凭据异常，相关账号已进入健康检查流程。";
    case "quota":
      return "上游账号额度不足或正在等待恢复，请稍后重试。";
    case "model":
      return "当前模型路由不可用，请检查模型授权、映射和账号能力。";
    case "egress":
      if (/browser[_ -]?worker|worker 容器/.test(`${message}`.toLowerCase())) return "浏览器 worker 不可用，请检查 worker 容器状态。";
      return "代理出口或 Cloudflare 会话不可用，请检查出口节点状态。";
    case "timeout":
      return /abort|cancel/i.test(raw) ? "请求已取消。" : "请求超时，上游响应过慢，请稍后重试。";
    case "rate":
      return "请求过于频繁，请稍后再试。";
    case "upstream":
      return raw || "上游服务异常，请稍后重试。";
    default:
      if (!raw || /^http\s*\d+/i.test(raw) || /empty response body/i.test(raw)) {
        if (status === 404) return "接口不存在或路径错误。";
        if (status && status >= 500) return "服务暂时不可用，请稍后重试。";
        if (/empty response body/i.test(raw)) return "服务器返回为空，请重试。";
        return raw || "请求失败，请稍后重试。";
      }
      if (/failed to fetch|networkerror|load failed/i.test(raw)) {
        return "网络连接失败，请检查服务是否在线。";
      }
      return raw;
  }
}

export function classifyGatewayError(status: number, body: unknown, fallback: string): ClassifiedError {
  const parsed = (body ?? {}) as OpenAIErrorBody;
  const rawMessage =
    parsed.error?.message ||
    parsed.message ||
    (typeof body === "string" && body.trim() ? body : fallback);
  const code = parsed.error?.code != null ? String(parsed.error.code) : parsed.error?.type;
  const errorClass = classifyErrorClass(status, code ?? "", String(rawMessage ?? ""));
  const requestId = parsed.error?.request_id || parsed.error?.requestId || parsed.request_id || parsed.requestId;

  return {
    class: errorClass,
    message: localizeGatewayMessage(errorClass, String(rawMessage || fallback), status),
    code,
    status,
    requestId,
  };
}
