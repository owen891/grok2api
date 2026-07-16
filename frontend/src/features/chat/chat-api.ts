import { runtimeConfig } from "@/shared/config/runtime-config";
import {
  imageModelForQuality,
  isSupportedImageUrl,
  type ChatErrorClass,
  type ImageSettings,
} from "./chat-types";
import { ChatSseUpstreamError, consumeChatSse } from "./chat-sse";

export type GatewayModel = {
  id: string;
  owned_by?: string;
};

export type ClassifiedError = {
  class: ChatErrorClass;
  message: string;
  code?: string;
  status?: number;
};

type OpenAIErrorBody = {
  error?: {
    message?: string;
    type?: string;
    code?: string | number;
  };
  message?: string;
};

function joinUrl(path: string): string {
  if (/^https?:\/\//i.test(path)) return path;
  const base = runtimeConfig.apiBaseUrl.replace(/\/$/, "");
  const suffix = path.startsWith("/") ? path : `/${path}`;
  return `${base}${suffix}`;
}

function authHeaders(clientKey: string, extra?: HeadersInit): Headers {
  const headers = new Headers(extra);
  headers.set("Authorization", `Bearer ${clientKey}`);
  return headers;
}

export function classifyGatewayError(status: number, body: unknown, fallback: string): ClassifiedError {
  const parsed = (body ?? {}) as OpenAIErrorBody;
  const rawMessage =
    parsed.error?.message ||
    parsed.message ||
    (typeof body === "string" && body.trim() ? body : fallback);
  const code = parsed.error?.code != null ? String(parsed.error.code) : parsed.error?.type;

  let errorClass: ChatErrorClass = "unknown";
  if (status === 401 || status === 403) errorClass = "auth";
  else if (status === 408 || status === 504) errorClass = "timeout";
  else if (status === 429) errorClass = "rate";
  else if (status === 402 || /spending|entitlement|quota|cpa|account/i.test(String(rawMessage))) errorClass = "upstream";
  else if (status >= 500) errorClass = "upstream";

  return {
    class: errorClass,
    message: localizeGatewayMessage(errorClass, String(rawMessage || fallback), status),
    code,
    status,
  };
}

function localizeGatewayMessage(errorClass: ChatErrorClass, message: string, status?: number): string {
  const raw = (message || "").trim();

  if (errorClass === "auth") {
    if (/invalid|unauthorized|forbidden|token|api key|client key|secret/i.test(raw)) {
      return "客户端密钥无效或已失效，请重新选择并确认密钥。";
    }
    return raw || "鉴权失败，请检查客户端密钥。";
  }
  if (errorClass === "timeout") {
    if (/abort|cancel/i.test(raw)) return "请求已取消。";
    return "请求超时，上游响应过慢，请稍后重试。";
  }
  if (errorClass === "rate") {
    return "请求过于频繁，请稍后再试。";
  }
  if (errorClass === "upstream") {
    if (/spending|entitlement|quota|cpa|account|balance|insufficient/i.test(raw)) {
      return "上游账户额度或权限不足，请检查 Grok 账号状态。";
    }
    if (/model|not found|unsupported/i.test(raw)) {
      return "当前模型不可用，请切换其他模型后重试。";
    }
    return raw || "上游服务异常，请稍后重试。";
  }
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



async function readError(response: Response): Promise<ClassifiedError> {
  let body: unknown;
  try {
    const text = await response.text();
    try {
      body = text ? JSON.parse(text) : null;
    } catch {
      body = text;
    }
  } catch {
    body = null;
  }
  return classifyGatewayError(response.status, body, `HTTP ${response.status}`);
}

export async function listGatewayModels(clientKey: string, signal?: AbortSignal): Promise<GatewayModel[]> {
  const response = await fetch(joinUrl("/v1/models"), {
    method: "GET",
    headers: authHeaders(clientKey),
    signal,
  });
  if (!response.ok) {
    throw await readError(response);
  }
  const payload = (await response.json()) as { data?: GatewayModel[] };
  return Array.isArray(payload.data) ? payload.data : [];
}

export type ChatCompletionMessage = {
  role: "system" | "user" | "assistant";
  content: string;
};

export async function streamChatCompletion(options: {
  clientKey: string;
  model: string;
  messages: ChatCompletionMessage[];
  signal?: AbortSignal;
  onDelta: (text: string) => void;
}): Promise<string> {
  const response = await fetch(joinUrl("/v1/chat/completions"), {
    method: "POST",
    headers: authHeaders(options.clientKey, { "Content-Type": "application/json" }),
    body: JSON.stringify({
      model: options.model,
      stream: true,
      messages: options.messages,
    }),
    signal: options.signal,
  });

  if (!response.ok) {
    throw await readError(response);
  }
  if (!response.body) {
    throw { class: "unknown", message: "服务器返回为空，请重试。" } satisfies ClassifiedError;
  }

  try {
    return await consumeChatSse(response.body, options.onDelta);
  } catch (error) {
    if (error instanceof ChatSseUpstreamError) {
      throw {
        class: "upstream",
        message: error.message,
        code: error.code,
      } satisfies ClassifiedError;
    }
    throw error;
  }
}

export type GeneratedImage = {
  url: string;
  mimeType?: string;
};

export async function generateImages(options: {
  clientKey: string;
  prompt: string;
  settings: ImageSettings;
  model?: string;
  signal?: AbortSignal;
}): Promise<GeneratedImage[]> {
  const model = options.model || imageModelForQuality(options.settings.quality);
  const response = await fetch(joinUrl("/v1/images/generations"), {
    method: "POST",
    headers: authHeaders(options.clientKey, { "Content-Type": "application/json" }),
    body: JSON.stringify({
      model,
      prompt: options.prompt,
      n: Math.min(4, Math.max(1, options.settings.n)),
      aspect_ratio: options.settings.aspectRatio,
      resolution: options.settings.resolution,
      response_format: "url",
    }),
    signal: options.signal,
  });

  if (!response.ok) {
    throw await readError(response);
  }

  const payload = (await response.json()) as {
    data?: Array<{ url?: string; b64_json?: string; mime_type?: string }>;
  };
  const items = Array.isArray(payload.data) ? payload.data : [];
  const images: GeneratedImage[] = [];
  for (const item of items) {
    if (item.url && isSupportedImageUrl(item.url)) {
      images.push({ url: item.url.trim(), mimeType: item.mime_type });
      continue;
    }
    if (item.b64_json) {
      const mime = item.mime_type && /^image\/[a-z0-9.+-]+$/i.test(item.mime_type) ? item.mime_type : "image/png";
      images.push({ url: `data:${mime};base64,${item.b64_json}`, mimeType: mime });
    }
  }
  return images;
}

export async function editImages(options: {
  clientKey: string;
  prompt: string;
  references: Array<{ url: string; mimeType?: string }>;
  settings: ImageSettings;
  model?: string;
  signal?: AbortSignal;
}): Promise<GeneratedImage[]> {
  const model = options.model || "grok-imagine-image-edit";
  const references = options.references.filter((reference) => isSupportedImageUrl(reference.url));
  const response = await fetch(joinUrl("/v1/images/edits"), {
    method: "POST",
    headers: authHeaders(options.clientKey, { "Content-Type": "application/json" }),
    body: JSON.stringify({
      model,
      prompt: options.prompt,
      images: references.map((reference) => ({ url: reference.url })),
      n: Math.min(4, Math.max(1, options.settings.n)),
      resolution: options.settings.resolution,
      response_format: "url",
    }),
    signal: options.signal,
  });

  if (!response.ok) throw await readError(response);
  const payload = (await response.json()) as {
    data?: Array<{ url?: string; b64_json?: string; mime_type?: string }>;
  };
  const images: GeneratedImage[] = [];
  for (const item of Array.isArray(payload.data) ? payload.data : []) {
    if (item.url && isSupportedImageUrl(item.url)) {
      images.push({ url: item.url.trim(), mimeType: item.mime_type });
    } else if (item.b64_json) {
      const mime = item.mime_type && /^image\/[a-z0-9.+-]+$/i.test(item.mime_type) ? item.mime_type : "image/png";
      images.push({ url: `data:${mime};base64,${item.b64_json}`, mimeType: mime });
    }
  }
  return images;
}

export function isAbortError(error: unknown): boolean {
  return (
    (error instanceof DOMException && error.name === "AbortError") ||
    (error instanceof Error && error.name === "AbortError")
  );
}

export function toClassifiedError(error: unknown, fallback: string): ClassifiedError {
  if (isAbortError(error)) {
    return { class: "timeout", message: localizeGatewayMessage("timeout", fallback, undefined), code: "aborted" };
  }
  if (error && typeof error === "object" && "class" in error && "message" in error) {
    return error as ClassifiedError;
  }
  if (error instanceof TypeError) {
    return { class: "timeout", message: localizeGatewayMessage("timeout", error.message || fallback, undefined) };
  }
  if (error instanceof Error) {
    return { class: "unknown", message: localizeGatewayMessage("unknown", error.message || fallback, undefined) };
  }
  return { class: "unknown", message: localizeGatewayMessage("unknown", fallback, undefined) };
}
