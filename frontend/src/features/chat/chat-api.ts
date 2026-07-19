import { runtimeConfig } from "@/shared/config/runtime-config";
import {
  imageModelForQuality,
  isSupportedImageUrl,
  type ImageSettings,
} from "./chat-types";
import {
  classifyGatewayError,
  localizeGatewayMessage,
  type ClassifiedError,
} from "./chat-error";
import { ChatSseUpstreamError, consumeChatSse } from "./chat-sse";

export { classifyGatewayError } from "./chat-error";
export type { ClassifiedError } from "./chat-error";

export type GatewayModel = {
  id: string;
  owned_by?: string;
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
  const classified = classifyGatewayError(response.status, body, `HTTP ${response.status}`);
  return {
    ...classified,
    requestId: classified.requestId || response.headers.get("X-Request-ID") || undefined,
  };
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

export type ChatCompletionContentPart =
  | { type: "text"; text: string }
  | { type: "image_url"; image_url: { url: string } };

export type ChatCompletionMessage = {
  role: "system" | "user" | "assistant";
  content: string | ChatCompletionContentPart[];
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
      throw classifyGatewayError(502, { error: { message: error.message, code: error.code } }, error.message);
    }
    throw error;
  }
}

export type GeneratedImage = {
  url: string;
  mimeType?: string;
};

type ImagePayloadItem = { url?: string; b64_json?: string; mime_type?: string };

export function decodeGeneratedImages(payload: unknown): GeneratedImage[] {
  const data = payload && typeof payload === "object" && "data" in payload
    ? (payload as { data?: unknown }).data
    : undefined;
  const items = Array.isArray(data) ? data as ImagePayloadItem[] : [];
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

export type ImageJob =
  | { status: "pending"; model?: string; progress: number }
  | { status: "done"; model?: string; progress: 100; images: GeneratedImage[] }
  | { status: "failed"; error: ClassifiedError };

export async function createImageJob(options: {
  clientKey: string;
  prompt: string;
  settings: ImageSettings;
  model?: string;
  signal?: AbortSignal;
}): Promise<{ requestId: string }> {
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
      async: true,
    }),
    signal: options.signal,
  });
  if (!response.ok) throw await readError(response);
  const payload = await response.json() as { request_id?: unknown };
  const requestId = typeof payload.request_id === "string" ? payload.request_id.trim() : "";
  if (!requestId) {
    throw { class: "upstream", message: "图片任务未返回 request_id" } satisfies ClassifiedError;
  }
  return { requestId };
}

export async function getImageJob(options: {
  clientKey: string;
  requestId: string;
  signal?: AbortSignal;
}): Promise<ImageJob> {
  const response = await fetch(joinUrl(`/v1/images/${encodeURIComponent(options.requestId)}`), {
    method: "GET",
    headers: authHeaders(options.clientKey),
    signal: options.signal,
  });
  if (!response.ok) throw await readError(response);
  const payload = await response.json() as Record<string, unknown>;
  if (payload.status === "done") {
    return {
      status: "done",
      model: typeof payload.model === "string" ? payload.model : undefined,
      progress: 100,
      images: decodeGeneratedImages(payload),
    };
  }
  if (payload.status === "failed") {
    const classified = classifyGatewayError(502, payload, "图片任务失败");
    return { status: "failed", error: { ...classified, requestId: classified.requestId || options.requestId } };
  }
  const progress = typeof payload.progress === "number" && Number.isFinite(payload.progress)
    ? Math.min(99, Math.max(0, Math.round(payload.progress)))
    : 0;
  return {
    status: "pending",
    model: typeof payload.model === "string" ? payload.model : undefined,
    progress,
  };
}

function pollDelay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException("Aborted", "AbortError"));
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      reject(new DOMException("Aborted", "AbortError"));
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, milliseconds);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

export async function waitForImageJob(options: {
  clientKey: string;
  requestId: string;
  signal?: AbortSignal;
  pollIntervalMs?: number;
  timeoutMs?: number;
  onProgress?: (progress: number) => void;
}): Promise<GeneratedImage[]> {
  const startedAt = Date.now();
  const timeoutMs = options.timeoutMs ?? 30 * 60 * 1000;
  let pollCount = 0;
  for (;;) {
    const job = await getImageJob(options);
    if (job.status === "done") return job.images;
    if (job.status === "failed") throw job.error;
    options.onProgress?.(job.progress);
    if (Date.now() - startedAt >= timeoutMs) {
      throw { class: "timeout", message: "图片任务轮询超时", code: "image_job_timeout", requestId: options.requestId } satisfies ClassifiedError;
    }
    pollCount += 1;
    const defaultDelay = pollCount < 10 ? 1000 : pollCount < 30 ? 2000 : 3000;
    await pollDelay(options.pollIntervalMs ?? defaultDelay, options.signal);
  }
}

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

  return decodeGeneratedImages(await response.json());
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
  return decodeGeneratedImages(await response.json());
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
