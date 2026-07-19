import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isRecordOf, isString } from "@/shared/api/decoder";

import type { AccountProvider } from "@/features/accounts/accounts-api";

export type AccountInspectionMode = "full" | "incremental" | "selected" | "recheck";
export type AccountInspectionStatus = "queued" | "running" | "completed" | "failed" | "cancelled";
export type AccountInspectionClassification = "pending" | "healthy" | "permission_denied" | "quota_exhausted" | "reauth" | "model_unavailable" | "probe_error" | "cancelled";
export type AccountInspectionAction = "keep" | "clear_health" | "require_reauth" | "update_quota" | "review";

export type AccountInspectionRunDTO = {
  id: string;
  provider: AccountProvider;
  modelRouteId: string;
  upstreamModel: string;
  mode: AccountInspectionMode;
  status: AccountInspectionStatus;
  includeDisabled: boolean;
  concurrency: number;
  total: number;
  completed: number;
  cancelRequested: boolean;
  errorMessage?: string;
  startedAt?: string;
  finishedAt?: string;
  createdAt: string;
  updatedAt: string;
};

export type AccountInspectionResultDTO = {
  accountId: string;
  provider: AccountProvider;
  accountName: string;
  accountEmail?: string;
  accountEnabled: boolean;
  accountUpdatedAt: string;
  model: string;
  classification: AccountInspectionClassification;
  suggestedAction: AccountInspectionAction;
  confidence: "low" | "medium" | "high";
  failureScope?: string;
  failureAction?: string;
  httpStatus: number;
  errorCode?: string;
  errorMessage?: string;
  attempts: number;
  latencyMilliseconds: number;
  quotaExhausted: boolean;
  freeQuotaExhausted: boolean;
  modelQuotaExhausted: boolean;
  credentialRejected: boolean;
  permanentAccountDenial: boolean;
  applyStatus: "pending" | "applying" | "applied" | "skipped" | "failed";
  applyAttempts: number;
  applyError?: string;
  appliedAction?: string;
  appliedAt?: string;
  updatedAt: string;
};

export type AccountInspectionDetailDTO = {
  run: AccountInspectionRunDTO;
  items: AccountInspectionResultDTO[];
  summary: Record<string, number>;
  page: number;
  pageSize: number;
  total: number;
};

const providerValidator = isOneOf("grok_build", "grok_web", "grok_console");
const runShape = {
  id: isString, provider: providerValidator, modelRouteId: isString, upstreamModel: isString,
  mode: isOneOf("full", "incremental", "selected", "recheck"), status: isOneOf("queued", "running", "completed", "failed", "cancelled"),
  includeDisabled: isBoolean, concurrency: isNumber, total: isNumber, completed: isNumber, cancelRequested: isBoolean,
  errorMessage: isOptional(isString), startedAt: isOptional(isString), finishedAt: isOptional(isString), createdAt: isString, updatedAt: isString,
} as const;
const runValidator = hasShape(runShape);
const resultValidator = hasShape({
  accountId: isString, provider: providerValidator, accountName: isString, accountEmail: isOptional(isString), accountEnabled: isBoolean,
  accountUpdatedAt: isString, model: isString,
  classification: isOneOf("pending", "healthy", "permission_denied", "quota_exhausted", "reauth", "model_unavailable", "probe_error", "cancelled"),
  suggestedAction: isOneOf("keep", "clear_health", "require_reauth", "update_quota", "review"), confidence: isOneOf("low", "medium", "high"),
  failureScope: isOptional(isString), failureAction: isOptional(isString), httpStatus: isNumber, errorCode: isOptional(isString), errorMessage: isOptional(isString),
  attempts: isNumber, latencyMilliseconds: isNumber, quotaExhausted: isBoolean, freeQuotaExhausted: isBoolean, modelQuotaExhausted: isBoolean,
  credentialRejected: isBoolean, permanentAccountDenial: isBoolean,
  applyStatus: isOneOf("pending", "applying", "applied", "skipped", "failed"), applyAttempts: isNumber, applyError: isOptional(isString),
  appliedAction: isOptional(isString), appliedAt: isOptional(isString), updatedAt: isString,
});
const decodeRun = createObjectDecoder<AccountInspectionRunDTO>("account inspection run", runShape);
const decodeRuns = createObjectDecoder<{ items: AccountInspectionRunDTO[] }>("account inspection runs", { items: isArrayOf(runValidator) });
const decodeDetail = createObjectDecoder<AccountInspectionDetailDTO>("account inspection detail", {
  run: runValidator, items: isArrayOf(resultValidator), summary: isRecordOf(isNumber), page: isNumber, pageSize: isNumber, total: isNumber,
});
export function listAccountInspectionRuns(provider: AccountProvider): Promise<{ items: AccountInspectionRunDTO[] }> {
  return apiRequest(`/api/admin/v1/account-inspections?provider=${provider}&limit=10`, {}, decodeRuns);
}

export function getAccountInspection(id: string, page = 1, pageSize = 500): Promise<AccountInspectionDetailDTO> {
  return apiRequest(`/api/admin/v1/account-inspections/${id}?page=${page}&pageSize=${pageSize}`, {}, decodeDetail);
}

export function startAccountInspection(input: {
  provider: AccountProvider;
  modelRouteId: string;
  mode: AccountInspectionMode;
  accountIds?: string[];
  classifications?: AccountInspectionClassification[];
  includeDisabled: boolean;
  concurrency: number;
}): Promise<AccountInspectionRunDTO> {
  return apiRequest("/api/admin/v1/account-inspections", { method: "POST", body: input }, decodeRun);
}

export function cancelAccountInspection(id: string): Promise<AccountInspectionRunDTO> {
  return apiRequest(`/api/admin/v1/account-inspections/${id}/cancel`, { method: "POST" }, decodeRun);
}
