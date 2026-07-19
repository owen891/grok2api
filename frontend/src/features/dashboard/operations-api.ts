import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString } from "@/shared/api/decoder";

export type RouteCapacityDTO = {
  routeId: string;
  publicModel: string;
  provider: "grok_build" | "grok_web" | "grok_console";
  upstreamModel: string;
  capability: string;
  quotaMode?: string;
  total: number;
  eligible: number;
  saturated: number;
  disabled: number;
  reauthRequired: number;
  quotaExhausted: number;
  recoveringSoon: number;
  cooling: number;
  modelCooling: number;
  unsupported: number;
  inFlight: number;
  totalSlots: number;
  availableSlots: number;
  unlimited: number;
  earliestRecovery?: string;
};

export type TaskStatusDTO = {
  name: string;
  state: "running" | "degraded" | "stopped";
  startedAt?: string;
  lastRunAt?: string;
  lastHeartbeatAt?: string;
  lastSuccessAt?: string;
  lastFailureAt?: string;
  nextRunAt?: string;
  consecutiveFailures: number;
  restartCount: number;
  lastError?: string;
};

export type OperationsDTO = {
  generatedAt: string;
  routes: RouteCapacityDTO[];
  tasks: TaskStatusDTO[];
  replenishment: {
    enabled: boolean;
    dryRun: boolean;
    scope: string;
    maxDailyRegistrations: number;
    predictive: boolean;
    targetEligible: number;
    minDemandRPM: number;
    demandWindowSeconds: number;
    verificationGraceSeconds: number;
    state: "idle" | "starting" | "running" | "verifying" | "cooling" | "failed";
    lastTriggerAt?: string;
    nextAttemptAt?: string;
    dailyStarts: number;
    lastError?: string;
    updatedAt?: string;
  };
};

const routeCapacityValidator = hasShape({
  routeId: isString, publicModel: isString, provider: isOneOf("grok_build", "grok_web", "grok_console"),
  upstreamModel: isString, capability: isString, quotaMode: isOptional(isString), total: isNumber, eligible: isNumber,
  saturated: isNumber, disabled: isNumber, reauthRequired: isNumber, quotaExhausted: isNumber, recoveringSoon: isNumber,
  cooling: isNumber, modelCooling: isNumber, unsupported: isNumber, inFlight: isNumber, totalSlots: isNumber,
  availableSlots: isNumber, unlimited: isNumber, earliestRecovery: isOptional(isString),
});

const taskStatusValidator = hasShape({
  name: isString, state: isOneOf("running", "degraded", "stopped"), startedAt: isOptional(isString),
  lastRunAt: isOptional(isString), lastHeartbeatAt: isOptional(isString), lastSuccessAt: isOptional(isString),
  lastFailureAt: isOptional(isString), nextRunAt: isOptional(isString), consecutiveFailures: isNumber,
  restartCount: isNumber, lastError: isOptional(isString),
});

const decodeOperations = createObjectDecoder<OperationsDTO>("operations", {
  generatedAt: isString,
  routes: isArrayOf(routeCapacityValidator),
  tasks: isArrayOf(taskStatusValidator),
  replenishment: hasShape({
    enabled: isBoolean, dryRun: isBoolean, scope: isString, maxDailyRegistrations: isNumber,
    predictive: isBoolean, targetEligible: isNumber, minDemandRPM: isNumber, demandWindowSeconds: isNumber, verificationGraceSeconds: isNumber,
    state: isOneOf("idle", "starting", "running", "verifying", "cooling", "failed"), lastTriggerAt: isOptional(isString),
    nextAttemptAt: isOptional(isString), dailyStarts: isNumber, lastError: isOptional(isString), updatedAt: isOptional(isString),
  }),
});

export function getOperations(): Promise<OperationsDTO> {
  return apiRequest("/api/admin/v1/operations", {}, decodeOperations);
}
