import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString, type ValueValidator } from "@/shared/api/decoder";

export type RegistrationFailureDTO = {
  code: string;
  message: string;
};

export type RegistrationProgressDTO = {
  mode: "count" | "extra" | "unlimited" | "";
  done: number;
  total: number | null;
  percent: number | null;
  indeterminate: boolean;
  accountCount: number;
  attempted: number;
  succeeded: number;
  failed: number;
  resumable: number;
};

export type RegistrationStatusDTO = {
  configured: boolean;
  running: boolean;
  pid?: number;
  startedAt?: string;
  finishedAt?: string;
  exitCode?: number;
  lastError?: RegistrationFailureDTO;
  progress: RegistrationProgressDTO;
};

export type RegistrationLogEntryDTO = {
  id: number;
  text: string;
};

export type RegistrationLogsDTO = {
  items: RegistrationLogEntryDTO[];
  nextLogId: number;
};

export type RegistrationSettingsDTO = {
  engine: string;
  emailProvider: string;
  emailProviderFallbacks: string[];
  tempmailLolApiBase: string;
  tempmailLolDomain: string;
  tempmailLolPrefix: string;
  proxy: string;
  cpaBaseURL: string;
  cpaProxy: string;
  cpaHeadless: boolean;
  cpaProbeChat: boolean;
  cpaCloseBrowserAfterAuth: boolean;
};

export type RegistrationPreflightCheckDTO = {
  name: string;
  ok: boolean;
  detail: string;
};

export type RegistrationPreflightDTO = {
  ok: boolean;
  checks: RegistrationPreflightCheckDTO[];
  config: RegistrationSettingsDTO;
};

export type RegistrationStartInput = {
  count: number;
  extra: number;
  threads: number;
  fast: boolean;
  accountType: "build" | "web";
};

const isNullable = (validator: ValueValidator): ValueValidator => (value) => value === null || validator(value);
const failureValidator = hasShape({ code: isString, message: isString });
const progressValidator = hasShape({
  mode: isOneOf("count", "extra", "unlimited", ""),
  done: isNumber,
  total: isNullable(isNumber),
  percent: isNullable(isNumber),
  indeterminate: isBoolean,
  accountCount: isNumber,
  attempted: isNumber,
  succeeded: isNumber,
  failed: isNumber,
  resumable: isNumber,
});
const statusDecoder = createObjectDecoder<RegistrationStatusDTO>("registration status", {
  configured: isBoolean,
  running: isBoolean,
  pid: isOptional(isNumber),
  startedAt: isOptional(isString),
  finishedAt: isOptional(isString),
  exitCode: isOptional(isNumber),
  lastError: isOptional(failureValidator),
  progress: progressValidator,
});
const logEntryValidator = hasShape({ id: isNumber, text: isString });
const logsDecoder = createObjectDecoder<RegistrationLogsDTO>("registration logs", {
  items: isArrayOf(logEntryValidator),
  nextLogId: isNumber,
});
const settingsShape = {
  engine: isString,
  emailProvider: isString,
  emailProviderFallbacks: isArrayOf(isString),
  tempmailLolApiBase: isString,
  tempmailLolDomain: isString,
  tempmailLolPrefix: isString,
  proxy: isString,
  cpaBaseURL: isString,
  cpaProxy: isString,
  cpaHeadless: isBoolean,
  cpaProbeChat: isBoolean,
  cpaCloseBrowserAfterAuth: isBoolean,
} as const;
const settingsDecoder = createObjectDecoder<RegistrationSettingsDTO>("registration settings", settingsShape);
const preflightCheckValidator = hasShape({ name: isString, ok: isBoolean, detail: isString });
const preflightDecoder = createObjectDecoder<RegistrationPreflightDTO>("registration preflight", {
  ok: isBoolean,
  checks: isArrayOf(preflightCheckValidator),
  config: hasShape(settingsShape),
});

export function getRegistrationStatus(signal?: AbortSignal): Promise<RegistrationStatusDTO> {
  return apiRequest("/api/admin/v1/registration", { signal }, statusDecoder);
}

export function getRegistrationLogs(limit = 500, signal?: AbortSignal): Promise<RegistrationLogsDTO> {
  return apiRequest(`/api/admin/v1/registration/logs?limit=${limit}`, { signal }, logsDecoder);
}

export function getRegistrationSettings(signal?: AbortSignal): Promise<RegistrationSettingsDTO> {
  return apiRequest("/api/admin/v1/registration/config", { signal }, settingsDecoder);
}

export function updateRegistrationSettings(input: RegistrationSettingsDTO): Promise<RegistrationSettingsDTO> {
  return apiRequest("/api/admin/v1/registration/config", { method: "PUT", body: input }, settingsDecoder);
}

export function getRegistrationPreflight(): Promise<RegistrationPreflightDTO> {
  return apiRequest("/api/admin/v1/registration/preflight", {}, preflightDecoder);
}

export function startRegistration(input: RegistrationStartInput): Promise<RegistrationStatusDTO> {
  return apiRequest("/api/admin/v1/registration/start", { method: "POST", body: input }, statusDecoder);
}

export function stopRegistration(): Promise<RegistrationStatusDTO> {
  return apiRequest("/api/admin/v1/registration/stop", { method: "POST" }, statusDecoder);
}
