import assert from "node:assert/strict";

import { ChatSseUpstreamError, consumeChatSse } from "../src/features/chat/chat-sse.ts";
import {
  imageSettingsForModel,
  imageSettingsForAvailableModels,
  normalizeImageSettings,
  QUALITY_IMAGE_MODEL,
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

await testFinalFrameAndUtf8ChunkBoundaries();
await testReaderIsReleasedOnUpstreamError();
testImageSettingsModelCompatibility();
console.log("chat SSE regression tests passed");
