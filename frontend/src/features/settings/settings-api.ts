import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, decodeBooleanResult, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString } from "@/shared/api/decoder";
import type { SortOrder } from "@/shared/lib/table-sort";

export type SettingsConfigDTO = {
  providerBuild: { baseURL: string; clientVersion: string; clientIdentifier: string; tokenAuth: string; tokenAuthConfigured: boolean; userAgent: string };
  providerWeb: {
    baseURL: string; quotaTimeout: string; chatTimeout: string; imageTimeout: string; videoTimeout: string;
    statsigMode: "manual" | "url"; statsigManualValue?: string; statsigManualConfigured: boolean; statsigSignerURL: string;
    mediaConcurrency: number; allowNSFW: boolean;
    recoveryBackoffBase: string; recoveryBackoffMax: string;
  };
  providerConsole: { baseURL: string; userAgent: string; chatTimeout: string };
  batch: { importConcurrency: number; conversionConcurrency: number; syncConcurrency: number; refreshConcurrency: number; randomDelay: string };
  media: {
    maxImageBytes: number; maxTotalBytes: number; cleanupThresholdPercent: number;
    cleanupInterval: string;
  };
  routing: { stickyTTL: string; cooldownBase: string; cooldownMax: string; capacityWait: string; maxAttempts: number };
  audit: { bufferSize: number; batchSize: number; flushInterval: string };
  clientKeyDefaults: { rpmLimit: number; maxConcurrent: number };
};

export type EgressNodeDTO = {
  id: string; name: string; scope: EgressScope; enabled: boolean;
  proxyConfigured: boolean; userAgent: string; cookieConfigured: boolean;
  health: number; failureCount: number; cooldownUntil?: string; lastError?: string;
};

export type EgressNodeInput = {
  name: string; scope: EgressScope; enabled: boolean; proxyURL?: string;
  clearProxyURL?: boolean; userAgent: string; cloudflareCookies?: string; clearCookies?: boolean;
};

export type EgressScope = "grok_build" | "grok_web" | "grok_console" | "grok_web_asset";
export type EgressNodeListDTO = { items: EgressNodeDTO[]; defaultUserAgents: Record<EgressScope, string> };
export type EgressGroupStrategy = "least_load" | "weighted" | "sticky" | "round_robin";
export type EgressGroupDTO = { id: string; name: string; scope: EgressScope; enabled: boolean; strategy: EgressGroupStrategy; maxConcurrency: number; fallbackGroupId?: string; memberCount: number; enabledMembers: number; createdAt: string; updatedAt: string };
export type EgressGroupInput = { name: string; scope: EgressScope; enabled: boolean; strategy: EgressGroupStrategy; maxConcurrency: number; fallbackGroupId?: string };
export type EgressGroupImportResultDTO = { line: number; proxyConfigured: boolean; nodeId?: string; created: boolean; reused: boolean; error?: string };

export type SettingsSnapshotDTO = {
  config: SettingsConfigDTO;
  recommendedProviderBuild: { clientVersion: string; userAgent: string };
  updatedAt: string;
  revision: string;
  restartRequired: string[];
};

const settingsConfigValidator = hasShape({
  providerBuild: hasShape({ baseURL: isString, clientVersion: isString, clientIdentifier: isString, tokenAuth: isString, tokenAuthConfigured: isBoolean, userAgent: isString }),
  providerWeb: hasShape({
    baseURL: isString, quotaTimeout: isString, chatTimeout: isString, imageTimeout: isString, videoTimeout: isString,
    statsigMode: isOneOf("manual", "url"), statsigManualValue: isOptional(isString), statsigManualConfigured: isBoolean,
    statsigSignerURL: isString, mediaConcurrency: isNumber, allowNSFW: isBoolean, recoveryBackoffBase: isString, recoveryBackoffMax: isString,
  }),
  batch: hasShape({ importConcurrency: isNumber, conversionConcurrency: isNumber, syncConcurrency: isNumber, refreshConcurrency: isNumber, randomDelay: isString }),
  media: hasShape({ maxImageBytes: isNumber, maxTotalBytes: isNumber, cleanupThresholdPercent: isNumber, cleanupInterval: isString }),
  routing: hasShape({ stickyTTL: isString, cooldownBase: isString, cooldownMax: isString, capacityWait: isString, maxAttempts: isNumber }),
  audit: hasShape({ bufferSize: isNumber, batchSize: isNumber, flushInterval: isString }),
  clientKeyDefaults: hasShape({ rpmLimit: isNumber, maxConcurrent: isNumber }),
});
const decodeSettingsSnapshot = createObjectDecoder<SettingsSnapshotDTO>("settings", {
  config: settingsConfigValidator,
  recommendedProviderBuild: hasShape({ clientVersion: isString, userAgent: isString }),
  updatedAt: isString,
  revision: isString,
  restartRequired: isArrayOf(isString),
});
const egressNodeValidator = hasShape({
  id: isString, name: isString, scope: isOneOf("grok_build", "grok_web", "grok_console", "grok_web_asset"), enabled: isBoolean,
  proxyConfigured: isBoolean, userAgent: isString, cookieConfigured: isBoolean, health: isNumber, failureCount: isNumber,
  cooldownUntil: isOptional(isString), lastError: isOptional(isString),
});
const decodeEgressNode = createObjectDecoder<EgressNodeDTO>("egress node", {
  id: isString, name: isString, scope: isOneOf("grok_build", "grok_web", "grok_console", "grok_web_asset"), enabled: isBoolean,
  proxyConfigured: isBoolean, userAgent: isString, cookieConfigured: isBoolean, health: isNumber, failureCount: isNumber,
  cooldownUntil: isOptional(isString), lastError: isOptional(isString),
});
const decodeEgressNodeList = createObjectDecoder<EgressNodeListDTO>("egress node list", {
  items: isArrayOf(egressNodeValidator),
  defaultUserAgents: hasShape({ grok_build: isString, grok_web: isString, grok_console: isString, grok_web_asset: isString }),
});
const egressGroupValidator = hasShape({
  id: isString, name: isString, scope: isOneOf("grok_build", "grok_web", "grok_console", "grok_web_asset"), enabled: isBoolean,
  strategy: isOneOf("least_load", "weighted", "sticky", "round_robin"), maxConcurrency: isNumber,
  fallbackGroupId: isOptional(isString), memberCount: isNumber, enabledMembers: isNumber, createdAt: isString, updatedAt: isString,
});
const decodeEgressGroup = createObjectDecoder<EgressGroupDTO>("egress group", {
  id: isString, name: isString, scope: isOneOf("grok_build", "grok_web", "grok_console", "grok_web_asset"), enabled: isBoolean,
  strategy: isOneOf("least_load", "weighted", "sticky", "round_robin"), maxConcurrency: isNumber,
  fallbackGroupId: isOptional(isString), memberCount: isNumber, enabledMembers: isNumber, createdAt: isString, updatedAt: isString,
});
const decodeEgressGroupList = createObjectDecoder<{ items: EgressGroupDTO[] }>("egress group list", { items: isArrayOf(egressGroupValidator) });
const importResultValidator = hasShape({ line: isNumber, proxyConfigured: isBoolean, nodeId: isOptional(isString), created: isBoolean, reused: isBoolean, error: isOptional(isString) });
const decodeEgressGroupImport = createObjectDecoder<{ items: EgressGroupImportResultDTO[] }>("egress group import", { items: isArrayOf(importResultValidator) });

export function getSettings(): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings", {}, decodeSettingsSnapshot);
}

export function updateSettings(revision: string, config: SettingsConfigDTO): Promise<SettingsSnapshotDTO> {
  return apiRequest("/api/admin/v1/settings", { method: "PUT", body: { revision, config } }, decodeSettingsSnapshot);
}

export function listEgressNodes(input?: { sortBy?: string; sortOrder?: SortOrder }): Promise<EgressNodeListDTO> {
  const query = new URLSearchParams();
  if (input?.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  const suffix = query.size > 0 ? `?${query}` : "";
  return apiRequest(`/api/admin/v1/egress-nodes${suffix}`, {}, decodeEgressNodeList);
}

export function createEgressNode(input: EgressNodeInput): Promise<EgressNodeDTO> {
  return apiRequest("/api/admin/v1/egress-nodes", { method: "POST", body: input }, decodeEgressNode);
}

export function updateEgressNode(id: string, input: EgressNodeInput): Promise<EgressNodeDTO> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}`, { method: "PUT", body: input }, decodeEgressNode);
}

export function deleteEgressNode(id: string): Promise<{ deleted: boolean }> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}`, { method: "DELETE" }, decodeBooleanResult<{ deleted: boolean }>("deleted"));
}

export function listEgressGroups(scope?: EgressScope): Promise<{ items: EgressGroupDTO[] }> {
  const suffix = scope ? `?scope=${encodeURIComponent(scope)}` : "";
  return apiRequest(`/api/admin/v1/egress-groups${suffix}`, {}, decodeEgressGroupList);
}

export function createEgressGroup(input: EgressGroupInput): Promise<EgressGroupDTO> {
  return apiRequest("/api/admin/v1/egress-groups", { method: "POST", body: input }, decodeEgressGroup);
}

export function updateEgressGroup(id: string, input: EgressGroupInput): Promise<EgressGroupDTO> {
  return apiRequest(`/api/admin/v1/egress-groups/${id}`, { method: "PUT", body: input }, decodeEgressGroup);
}

export function deleteEgressGroup(id: string): Promise<{ deleted: boolean }> {
  return apiRequest(`/api/admin/v1/egress-groups/${id}`, { method: "DELETE" }, decodeBooleanResult<{ deleted: boolean }>("deleted"));
}

export function importEgressGroup(id: string, content: string, dryRun = false): Promise<{ items: EgressGroupImportResultDTO[] }> {
  return apiRequest(`/api/admin/v1/egress-groups/${id}/import`, { method: "POST", body: { content, dryRun, defaults: { enabled: true, weight: 1, maxConcurrency: 0, priority: 0 } } }, decodeEgressGroupImport);
}
