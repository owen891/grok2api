import { AlertTriangle, CheckCircle2, CirclePause, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { OperationsDTO, RouteCapacityDTO, TaskStatusDTO } from "@/features/dashboard/operations-api";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

export function OperationsPanels({ value, loading, locale }: { value?: OperationsDTO; loading: boolean; locale: string }) {
  const { t } = useTranslation();
  return (
    <>
      <section className="rounded-lg bg-card p-4 sm:p-5">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h2 className="text-sm font-medium">{t("dashboardOperations.capacityTitle")}</h2>
          <ReplenishmentStatus value={value?.replenishment} locale={locale} />
        </div>
        <div className="mt-4 overflow-x-auto">
          <Table className="min-w-[980px] table-fixed text-xs">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-[24%]">{t("dashboard.model")}</TableHead>
                <TableHead className="w-[13%]">{t("dashboardOperations.route")}</TableHead>
                <TableHead className="w-[12%]">{t("dashboardOperations.state")}</TableHead>
                <TableHead className="w-[12%] text-right">{t("dashboardOperations.accounts")}</TableHead>
                <TableHead className="w-[15%] text-right">{t("dashboardOperations.slots")}</TableHead>
                <TableHead className="w-[14%] text-right">{t("dashboardOperations.blocked")}</TableHead>
                <TableHead className="w-[10%] text-right">{t("dashboardOperations.recovery")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading ? <TableRow><TableCell colSpan={7} className="h-24 text-center"><Spinner /></TableCell></TableRow> : null}
              {!loading && (value?.routes.length ?? 0) === 0 ? <TableRow><TableCell colSpan={7} className="h-24 text-center text-muted-foreground">{t("dashboardOperations.noRoutes")}</TableCell></TableRow> : null}
              {!loading ? value?.routes.map((route) => <RouteCapacityRow key={route.routeId} value={route} locale={locale} />) : null}
            </TableBody>
          </Table>
        </div>
      </section>

      <section className="rounded-lg bg-card p-4 sm:p-5">
        <h2 className="text-sm font-medium">{t("dashboardOperations.tasksTitle")}</h2>
        <div className="mt-4 overflow-x-auto">
          <Table className="min-w-[860px] table-fixed text-xs">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-[28%]">{t("dashboardOperations.task")}</TableHead>
                <TableHead className="w-[13%]">{t("dashboardOperations.state")}</TableHead>
                <TableHead className="w-[20%]">{t("dashboardOperations.heartbeat")}</TableHead>
                <TableHead className="w-[20%]">{t("dashboardOperations.nextRun")}</TableHead>
                <TableHead className="w-[19%]">{t("dashboardOperations.failure")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading ? <TableRow><TableCell colSpan={5} className="h-24 text-center"><Spinner /></TableCell></TableRow> : null}
              {!loading ? value?.tasks.map((task) => <TaskStatusRow key={task.name} value={task} locale={locale} />) : null}
            </TableBody>
          </Table>
        </div>
      </section>
    </>
  );
}

function RouteCapacityRow({ value, locale }: { value: RouteCapacityDTO; locale: string }) {
  const { t } = useTranslation();
  const blocked = value.quotaExhausted + value.cooling + value.modelCooling + value.reauthRequired + value.unsupported + value.disabled;
  return (
    <TableRow>
      <TableCell className="py-3"><span className="block truncate font-medium" title={value.publicModel}>{value.publicModel}</span><span className="mt-0.5 block truncate text-[10px] text-muted-foreground" title={value.upstreamModel}>{value.upstreamModel}</span></TableCell>
      <TableCell className="py-3"><span className="block">{providerLabel(value.provider)}</span><span className="text-[10px] text-muted-foreground">{value.quotaMode || value.capability}</span></TableCell>
      <TableCell className="py-3"><CapacityBadge value={value} /></TableCell>
      <TableCell className="py-3 text-right tabular-nums">{formatNumber(value.eligible, locale, 0)} / {formatNumber(value.total, locale, 0)}</TableCell>
      <TableCell className="py-3 text-right"><span className="block tabular-nums">{formatNumber(value.availableSlots, locale, 0)} / {formatNumber(value.totalSlots, locale, 0)}</span><span className="text-[10px] text-muted-foreground">{t("dashboardOperations.inFlight", { count: formatNumber(value.inFlight, locale, 0) })}</span></TableCell>
      <TableCell className="py-3 text-right"><span className="block tabular-nums">{formatNumber(blocked, locale, 0)}</span><span className="text-[10px] text-muted-foreground">{t("dashboardOperations.blockedBreakdown", { quota: value.quotaExhausted, cooling: value.cooling + value.modelCooling, auth: value.reauthRequired })}</span></TableCell>
      <TableCell className="py-3 text-right text-muted-foreground">{value.earliestRecovery ? formatDateTime(value.earliestRecovery, locale) : "-"}</TableCell>
    </TableRow>
  );
}

function CapacityBadge({ value }: { value: RouteCapacityDTO }) {
  const { t } = useTranslation();
  if (value.eligible > 0) return <Badge className="bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">{t("dashboardOperations.healthy")}</Badge>;
  if (value.saturated > 0) return <Badge className="bg-sky-500/10 text-sky-700 dark:text-sky-300">{t("dashboardOperations.saturated")}</Badge>;
  if (value.quotaExhausted > 0) return <Badge className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("dashboardOperations.exhausted")}</Badge>;
  return <Badge variant="destructive">{t("dashboardOperations.unavailable")}</Badge>;
}

function TaskStatusRow({ value, locale }: { value: TaskStatusDTO; locale: string }) {
  const { t } = useTranslation();
  return (
    <TableRow>
      <TableCell className="py-3 font-mono text-[11px]">{value.name}</TableCell>
      <TableCell className="py-3"><TaskBadge state={value.state} /></TableCell>
      <TableCell className="py-3 text-muted-foreground">{formatDateTime(value.lastHeartbeatAt, locale)}</TableCell>
      <TableCell className="py-3 text-muted-foreground">{formatDateTime(value.nextRunAt, locale)}</TableCell>
      <TableCell className="py-3"><span className={cn("block truncate", value.lastError && "text-destructive")} title={value.lastError}>{value.lastError || t("dashboardOperations.none")}</span>{value.consecutiveFailures > 0 ? <span className="text-[10px] text-muted-foreground">{t("dashboardOperations.failureCount", { count: value.consecutiveFailures })}</span> : null}</TableCell>
    </TableRow>
  );
}

function TaskBadge({ state }: { state: TaskStatusDTO["state"] }) {
  const { t } = useTranslation();
  const Icon = state === "running" ? CheckCircle2 : state === "degraded" ? AlertTriangle : CirclePause;
  return <Badge variant={state === "degraded" ? "destructive" : "outline"} className={cn("gap-1", state === "running" && "border-emerald-500/35 text-emerald-700 dark:text-emerald-300")}><Icon className="size-3" />{t(`dashboardOperations.${state}`)}</Badge>;
}

function ReplenishmentStatus({ value, locale }: { value?: OperationsDTO["replenishment"]; locale: string }) {
  const { t } = useTranslation();
  if (!value) return <Spinner className="size-4" />;
  if (!value.enabled) return <Badge variant="outline" className="text-muted-foreground">{t("dashboardOperations.replenishmentDisabled")}</Badge>;
  return (
    <div className="flex flex-wrap items-center justify-end gap-2 text-[11px] text-muted-foreground">
      <Badge variant={value.state === "failed" ? "destructive" : "outline"} className="gap-1"><RefreshCw className={cn("size-3", (value.state === "starting" || value.state === "running" || value.state === "verifying") && "animate-spin")} />{t(`dashboardOperations.replenishment_${value.state}`)}</Badge>
      <span>{t("dashboardOperations.dailyStarts", { used: value.dailyStarts, limit: value.maxDailyRegistrations })}</span>
      {value.predictive ? <span>{t("dashboardOperations.predictiveThreshold", { eligible: value.targetEligible, rpm: formatNumber(value.minDemandRPM, locale, 1), minutes: formatNumber(value.demandWindowSeconds / 60, locale, 0) })}</span> : null}
      {value.nextAttemptAt ? <span>{formatDateTime(value.nextAttemptAt, locale)}</span> : null}
    </div>
  );
}

function providerLabel(value: RouteCapacityDTO["provider"]): string {
  if (value === "grok_build") return "Build";
  if (value === "grok_console") return "Console";
  return "Web";
}
