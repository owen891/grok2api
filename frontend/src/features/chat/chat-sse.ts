export class ChatSseUpstreamError extends Error {
  readonly code?: string;

  constructor(message: string, code?: string) {
    super(message);
    this.name = "ChatSseUpstreamError";
    this.code = code;
  }
}

type SsePayload = {
  choices?: Array<{
    delta?: { content?: string | Array<{ text?: string }> };
    message?: { content?: string };
  }>;
  error?: { message?: string; code?: string };
};

function processSseLine(line: string, onDelta: (text: string) => void): void {
  const trimmed = line.trim();
  if (!trimmed.startsWith("data:")) return;
  const data = trimmed.slice(5).trim();
  if (!data || data === "[DONE]") return;
  try {
    const parsed = JSON.parse(data) as SsePayload;
    if (parsed.error?.message) {
      throw new ChatSseUpstreamError(parsed.error.message, parsed.error.code);
    }
    const delta = parsed.choices?.[0]?.delta?.content;
    let piece = "";
    if (typeof delta === "string") {
      piece = delta;
    } else if (Array.isArray(delta)) {
      piece = delta.map((part) => (typeof part.text === "string" ? part.text : "")).join("");
    } else if (typeof parsed.choices?.[0]?.message?.content === "string") {
      piece = parsed.choices[0].message.content;
    }
    if (piece) onDelta(piece);
  } catch (error) {
    if (error instanceof ChatSseUpstreamError) throw error;
    // Ignore malformed SSE heartbeats and comments.
  }
}

export async function consumeChatSse(
  body: ReadableStream<Uint8Array>,
  onDelta: (text: string) => void,
): Promise<string> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let full = "";
  const emit = (piece: string) => {
    full += piece;
    onDelta(piece);
  };
  const processLine = (line: string) => processSseLine(line, emit);

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const chunks = buffer.split("\n");
      buffer = chunks.pop() ?? "";
      chunks.forEach(processLine);
    }
    buffer += decoder.decode();
    if (buffer) processLine(buffer);
    return full;
  } finally {
    reader.releaseLock();
  }
}
