import assert from "node:assert/strict";

import { classifyGatewayError } from "../src/features/chat/chat-error.ts";
import { ChatSseUpstreamError, consumeChatSse } from "../src/features/chat/chat-sse.ts";
import {
  formatGenerationMeta,
  imageSettingsForModel,
  imageSettingsForAvailableModels,
  isUsageLimitError,
  normalizeImageSettings,
  QUALITY_IMAGE_MODEL,
  sanitizeGenerationMeta,
  sanitizePersistedMessageTask,
  SPEED_IMAGE_MODEL,
  type ImageSettings,
} from "../src/features/chat/chat-types.ts";

function streamFromChunks(chunks: Uint8Array[]): ReadableStream<Uint8Array> {
  return new ReadableStream({
    start(controller) {
      chunks.forEach((chunk) => controller.enqueue(chunk));
      controller.close();
    },
  });
}

function encode(value: string): Uint8Array {
  return new TextEncoder().encode(value);
}

async function testFinalFrameAndUtf8ChunkBoundaries(): Promise<void> {
  const first = `data: ${JSON.stringify({ choices: [{ delta: { content: "你好" } }] })}\n\n`;
  const second = `data: ${JSON.stringify({ choices: [{ delta: { content: " world" } }] })}`;
  const bytes = encode(first + second);
  const body = streamFromChunks([bytes.slice(0, 7), bytes.slice(7, 10), bytes.slice(10)]);
  const pieces: string[] = [];

  const result = await consumeChatSse(body, (piece) => pieces.push(piece));

  assert.equal(result, "你好 world");
  assert.deepEqual(pieces, ["你好", " world"]);
  const reader = body.getReader();
  reader.releaseLock();
}

async function testReaderIsReleasedOnUpstreamError(): Promise<void> {
  const body = streamFromChunks([
    encode(`data: ${JSON.stringify({ error: { message: "upstream failed", code: "E_UPSTREAM" } })}`),
  ]);

  await assert.rejects(
    consumeChatSse(body, () => undefined),
    (error: unknown) => error instanceof ChatSseUpstreamError && error.code === "E_UPSTREAM",
  );
  const reader = body.getReader();
  reader.releaseLock();
}

function testImageSettingsModelCompatibility(): void {
  const fast: ImageSettings = { quality: "speed", n: 1, aspectRatio: "1:1", resolution: "1k" };
  assert.deepEqual(normalizeImageSettings(fast), fast);
  assert.equal(normalizeImageSettings({ ...fast, aspectRatio: "16:9" }).quality, "quality");
  assert.equal(normalizeImageSettings({ ...fast, resolution: "2k" }).quality, "quality");
  assert.deepEqual(imageSettingsForModel({ ...fast, aspectRatio: "16:9", resolution: "2k" }, SPEED_IMAGE_MODEL), fast);
  assert.equal(imageSettingsForModel(fast, QUALITY_IMAGE_MODEL).quality, "quality");
  assert.deepEqual(
    imageSettingsForAvailableModels({ ...fast, aspectRatio: "16:9" }, [SPEED_IMAGE_MODEL]),
    fast,
  );
  assert.equal(
    imageSettingsForAvailableModels({ ...fast, aspectRatio: "16:9" }, [SPEED_IMAGE_MODEL, QUALITY_IMAGE_MODEL]).quality,
    "quality",
  );
}

function testGenerationMetadataKeepsActualRouteContext(): void {
  const meta = sanitizeGenerationMeta({
    mode: "chat",
    model: "grok-4.5",
    clientKeyId: "1",
    clientKeyName: "chat-ui",
    clientKeyPrefix: "184fd9b0c80e",
  });
  assert.deepEqual(meta, {
    mode: "chat",
    model: "grok-4.5",
    clientKeyId: "1",
    clientKeyName: "chat-ui",
    clientKeyPrefix: "184fd9b0c80e",
    n: undefined,
    aspectRatio: undefined,
    resolution: undefined,
    quality: undefined,
  });
  assert.equal(formatGenerationMeta(meta), "对话 · Grok 4.5 · 密钥 · chat-ui (184fd9b0c80e...)");
}

function testGatewayErrorClassificationUsesStableCodes(): void {
  assert.deepEqual(
    classifyGatewayError(403, {
      error: {
        code: "upstream_account_permission_denied",
        message: "上游账号无权访问当前接口",
        request_id: "req-account",
      },
    }, "failed"),
    {
      class: "account",
      message: "上游账号权限或凭据异常，相关账号已进入健康检查流程。",
      code: "upstream_account_permission_denied",
      status: 403,
      requestId: "req-account",
    },
  );
  assert.equal(classifyGatewayError(403, { error: { code: "model_not_allowed" } }, "failed").class, "model");
  assert.equal(classifyGatewayError(429, { error: { code: "upstream_quota_exhausted" } }, "failed").class, "quota");
  assert.equal(classifyGatewayError(429, { error: { code: "usage_limit_reached" } }, "failed").class, "quota");
  assert.equal(classifyGatewayError(400, { error: { code: "image_moderated" } }, "failed").class, "moderation");
  assert.equal(classifyGatewayError(503, { error: { code: "egress_unavailable" } }, "failed").class, "egress");
  assert.equal(classifyGatewayError(401, { error: { code: "invalid_api_key" } }, "failed").class, "auth");
  assert.equal(classifyGatewayError(403, { error: { code: "upstream_forbidden" } }, "failed").class, "upstream");
}

function testPersistedTaskRecoveryRules(): void {
  assert.deepEqual(sanitizePersistedMessageTask({
    kind: "image",
    status: "running",
    requestId: " image_job_1 ",
    progress: 41.6,
  }), {
    kind: "image",
    status: "running",
    requestId: "image_job_1",
    progress: 42,
  });
  assert.equal(sanitizePersistedMessageTask({ kind: "chat", status: "running" })?.status, "cancelled");
  assert.equal(sanitizePersistedMessageTask({ kind: "image", status: "queued" })?.status, "cancelled");
}

await testFinalFrameAndUtf8ChunkBoundaries();
await testReaderIsReleasedOnUpstreamError();
testImageSettingsModelCompatibility();
testGenerationMetadataKeepsActualRouteContext();
testGatewayErrorClassificationUsesStableCodes();
testPersistedTaskRecoveryRules();
assert.equal(isUsageLimitError("usage_limit_reached", "请求过于频繁"), true);
console.log("chat SSE regression tests passed");
