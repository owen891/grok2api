import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Activity, CheckCircle2, ChevronLeft, ChevronRight, CircleStop, Play, ScanSearch } from "lucide-react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { listModels } from "@/entities/model/model-api";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";

import type { AccountProvider } from "@/features/accounts/accounts-api";
import {
  cancelAccountInspection,
  getAccountInspection,
  listAccountInspectionRuns,
  startAccountInspection,
  type AccountInspectionClassification,
  type AccountInspectionMode,
  type AccountInspectionResultDTO,
  type AccountInspectionRunDTO,
} from "@/features/accounts/account-inspection-api";

type Props = {
  provider: AccountProvider;
  selectedIds: string[];
};

const recheckOptions: AccountInspectionClassification[] = ["permission_denied", "quota_exhausted", "reauth", "model_unavailable", "probe_error"];

export function AccountInspectionDialog({ provider, selectedIds }: Props) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [view, setView] = useState<"start" | "results">("start");
  const [mode, setMode] = useState<AccountInspectionMode>(selectedIds.length > 0 ? "selected" : "full");
  const [modelRouteId, setModelRouteId] = useState("");
  const [includeDisabled, setIncludeDisabled] = useState(false);
  const [concurrency, setConcurrency] = useState("4");
  const [classifications, setClassifications] = useState<Set<AccountInspectionClassification>>(() => new Set(["quota_exhausted", "reauth", "probe_error"]));
  const [runId, setRunId] = useState("");
  const [page, setPage] = useState(1);

  const modelsQuery = useQuery({
    queryKey: ["account-inspection", "models", provider],
    queryFn: () => listModels({ page: 1, pageSize: 100, provider, status: "enabled", sortBy: "publicId", sortOrder: "asc" }),
    enabled: open,
  });
  const probeModels = useMemo(() => (modelsQuery.data?.items ?? []).filter((model) => model.enabled && model.available && (model.capability === "responses" || model.capability === "chat")), [modelsQuery.data]);

  const effectiveModelRouteId = probeModels.some((model) => model.id === modelRouteId) ? modelRouteId : (probeModels[0]?.id ?? "");

  const runsQuery = useQuery({
    queryKey: ["account-inspections", provider],
    queryFn: () => listAccountInspectionRuns(provider),
    enabled: open,
    refetchInterval: (query) => query.state.data?.items.some((run) => run.status === "queued" || run.status === "running") ? 2000 : false,
  });

  const effectiveRunId = runId || runsQuery.data?.items[0]?.id || "";

  const detailQuery = useQuery({
    queryKey: ["account-inspection", effectiveRunId, page],
    queryFn: () => getAccountInspection(effectiveRunId, page, 100),
    enabled: open && view === "results" && effectiveRunId !== "",
    refetchInterval: (query) => {
      const status = query.state.data?.run.status;
      return status === "queued" || status === "running" ? 2000 : false;
    },
  });

  const startMutation = useMutation({
    mutationFn: () => {
      if (!effectiveModelRouteId) throw new Error(t("accountInspection.noModel"));
      const parsedConcurrency = Number(concurrency);
      if (!Number.isInteger(parsedConcurrency) || parsedConcurrency < 1 || parsedConcurrency > 8) {
        throw new Error(t("accountInspection.invalidConcurrency"));
      }
      if (mode === "selected" && selectedIds.length === 0) throw new Error(t("accountInspection.noSelectedAccounts"));
      if (mode === "recheck" && classifications.size === 0) throw new Error(t("accountInspection.noClassifications"));
      return startAccountInspection({
        provider, modelRouteId: effectiveModelRouteId, mode,
        accountIds: mode === "selected" ? selectedIds : undefined,
        classifications: mode === "recheck" ? [...classifications] : undefined,
        includeDisabled, concurrency: parsedConcurrency,
      });
    },
    onSuccess: (run) => {
      setRunId(run.id);
      setPage(1);
      setOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["account-inspections", provider] });
      toast.success(t("accountInspection.started"));
    },
    onError: showError,
  });

  const cancelMutation = useMutation({
    mutationFn: cancelAccountInspection,
    onSuccess: (run) => {
      setRunId(run.id);
      void queryClient.invalidateQueries({ queryKey: ["account-inspection", run.id] });
      void queryClient.invalidateQueries({ queryKey: ["account-inspections", provider] });
      toast.success(t("accountInspection.cancelRequested"));
    },
    onError: showError,
  });

  const detail = detailQuery.data;
  const run = detail?.run ?? runsQuery.data?.items.find((item) => item.id === effectiveRunId);
  const active = run?.status === "queued" || run?.status === "running";
  const pageCount = Math.max(1, Math.ceil((detail?.total ?? 0) / 100));
  const canStart = !modelsQuery.isPending && !modelsQuery.isError && effectiveModelRouteId !== "" && !(mode === "selected" && selectedIds.length === 0) && !(mode === "recheck" && classifications.size === 0);

  function openDialog() {
    setOpen(true);
    setRunId("");
    setPage(1);
    setView("start");
    setMode(selectedIds.length > 0 ? "selected" : "full");
  }

  function toggleClassification(value: AccountInspectionClassification, checked: boolean) {
    setClassifications((current) => {
      const next = new Set(current);
      if (checked) next.add(value);
      else next.delete(value);
      return next;
    });
  }

  function showError(error: unknown) {
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
  }

  return (
    <>
      <Button variant="secondary" size="sm" onClick={openDialog}><ScanSearch />{t("accountInspection.action")}</Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className={cn("max-w-[1120px] overflow-hidden p-0", view === "results" && "h-[90vh] max-h-[820px]")}>
          <div className={cn("flex min-h-0 flex-col", view === "results" && "h-full")}>
            <DialogHeader className="shrink-0 border-b px-5 py-4">
              <DialogTitle className="flex items-center gap-2"><Activity className="size-4" />{t("accountInspection.title")}</DialogTitle>
              <DialogDescription className="sr-only">{t("accountInspection.title")}</DialogDescription>
            </DialogHeader>
            <div className="flex shrink-0 items-center justify-between gap-3 border-b px-5 py-2">
              <Tabs value={view} onValueChange={(value) => setView(value as "start" | "results")}>
                <TabsList>
                  <TabsTrigger value="start">{t("accountInspection.newRun")}</TabsTrigger>
                  <TabsTrigger value="results" disabled={!effectiveRunId}>{t("accountInspection.results")}</TabsTrigger>
                </TabsList>
              </Tabs>
              {run ? <RunStatusBadge run={run} /> : null}
            </div>

            {view === "start" ? (
              <div className="min-h-0 overflow-y-auto px-5 py-5">
                <div className="grid gap-5 md:grid-cols-2">
                  <div className="space-y-2">
                    <Label>{t("accountInspection.model")}</Label>
                    <Select value={effectiveModelRouteId} onValueChange={setModelRouteId} disabled={modelsQuery.isPending || modelsQuery.isError || probeModels.length === 0}>
                      <SelectTrigger><SelectValue placeholder={modelsQuery.isPending ? t("common.loading") : modelsQuery.isError ? t("accountInspection.modelsLoadFailed") : t("accountInspection.noModel")} /></SelectTrigger>
                      <SelectContent>{probeModels.map((model) => <SelectItem key={model.id} value={model.id}>{model.publicId}</SelectItem>)}</SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-2">
                    <Label>{t("accountInspection.concurrency")}</Label>
                    <Select value={concurrency} onValueChange={setConcurrency}>
                      <SelectTrigger><SelectValue /></SelectTrigger>
                      <SelectContent>{[1, 2, 4, 6, 8].map((value) => <SelectItem key={value} value={String(value)}>{value}</SelectItem>)}</SelectContent>
                    </Select>
                  </div>
                </div>

                <div className="mt-5 space-y-2">
                  <Label>{t("accountInspection.mode")}</Label>
                  <Tabs value={mode} onValueChange={(value) => setMode(value as AccountInspectionMode)}>
                    <TabsList className="max-w-full overflow-x-auto">
                      <TabsTrigger value="full">{t("accountInspection.modeFull")}</TabsTrigger>
                      <TabsTrigger value="incremental">{t("accountInspection.modeIncremental")}</TabsTrigger>
                      {selectedIds.length > 0 ? <TabsTrigger value="selected">{t("accountInspection.modeSelected", { count: selectedIds.length })}</TabsTrigger> : null}
                      <TabsTrigger value="recheck">{t("accountInspection.modeRecheck")}</TabsTrigger>
                    </TabsList>
                  </Tabs>
                </div>

                {mode === "recheck" ? (
                  <div className="mt-5 grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
                    {recheckOptions.map((classification) => (
                      <label key={classification} className="flex min-h-9 items-center gap-2 rounded-md border px-3 text-xs">
                        <Checkbox checked={classifications.has(classification)} onCheckedChange={(checked) => toggleClassification(classification, checked === true)} />
                        {t(`accountInspection.classification.${classification}`)}
                      </label>
                    ))}
                  </div>
                ) : null}

                <label className="mt-5 flex min-h-9 items-center gap-2 text-xs">
                  <Checkbox checked={includeDisabled} onCheckedChange={(checked) => setIncludeDisabled(checked === true)} />
                  {t("accountInspection.includeDisabled")}
                </label>
              </div>
            ) : (
              <InspectionResults detail={detail} loading={detailQuery.isPending} />
            )}

            <DialogFooter className="shrink-0 border-t px-5 py-3">
              {view === "start" ? (
                <Button disabled={!canStart || startMutation.isPending || active} onClick={() => startMutation.mutate()}>
                  {startMutation.isPending ? <Spinner /> : <Play />}{t("accountInspection.start")}
                </Button>
              ) : (
                <div className="flex w-full flex-wrap items-center justify-between gap-2">
                  <div className="flex items-center gap-1">
                    <Tooltip><TooltipTrigger asChild><Button size="icon" variant="ghost" disabled={page <= 1} onClick={() => setPage((value) => Math.max(1, value - 1))}><ChevronLeft /></Button></TooltipTrigger><TooltipContent>{t("common.previousPage")}</TooltipContent></Tooltip>
                    <span className="min-w-20 text-center text-xs text-muted-foreground">{t("common.pageOf", { page, pages: pageCount })}</span>
                    <Tooltip><TooltipTrigger asChild><Button size="icon" variant="ghost" disabled={page >= pageCount} onClick={() => setPage((value) => Math.min(pageCount, value + 1))}><ChevronRight /></Button></TooltipTrigger><TooltipContent>{t("common.nextPage")}</TooltipContent></Tooltip>
                  </div>
                  <div className="flex items-center gap-2">
                    {active && run ? <Button variant="secondary" disabled={cancelMutation.isPending || run.cancelRequested} onClick={() => cancelMutation.mutate(run.id)}>{cancelMutation.isPending ? <Spinner /> : <CircleStop />}{t("accountInspection.stop")}</Button> : null}
                  </div>
                </div>
              )}
            </DialogFooter>
          </div>
        </DialogContent>
      </Dialog>
    </>
  );
}

function InspectionResults({ detail, loading }: { detail?: { run: AccountInspectionRunDTO; items: AccountInspectionResultDTO[]; summary: Record<string, number>; total: number }; loading: boolean }) {
  const { t, i18n } = useTranslation();
  if (loading && !detail) return <div className="flex min-h-80 items-center justify-center"><Spinner /></div>;
  if (!detail) return <div className="flex min-h-80 items-center justify-center text-xs text-muted-foreground">{t("accountInspection.noResults")}</div>;
  const progress = detail.run.total > 0 ? Math.min(100, (detail.run.completed / detail.run.total) * 100) : 0;
  return (
    <div className="min-h-0 min-w-0 flex-1 overflow-y-auto px-5 py-4">
      <div className="mb-4 space-y-2">
        <div className="flex flex-wrap items-center justify-between gap-2 text-xs">
          <span className="font-medium">{detail.run.upstreamModel}</span>
          <span className="text-muted-foreground">{detail.run.completed} / {detail.run.total}</span>
        </div>
        <div className="h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary transition-[width]" style={{ width: `${progress}%` }} /></div>
        <div className="flex flex-wrap gap-1.5">
          {Object.entries(detail.summary).map(([classification, count]) => <Badge key={classification} variant={classificationVariant(classification)}>{t(`accountInspection.classification.${classification}`)} {count}</Badge>)}
        </div>
        {detail.run.errorMessage ? <p className="text-xs text-destructive">{detail.run.errorMessage}</p> : null}
      </div>
      <div className="max-w-full overflow-x-auto border">
        <Table className="min-w-[900px] table-fixed">
          <colgroup><col className="w-[190px]" /><col className="w-[100px]" /><col className="w-[60px]" /><col className="w-[310px]" /><col className="w-[150px]" /><col className="w-[150px]" /></colgroup>
          <TableHeader><TableRow><TableHead>{t("accounts.account")}</TableHead><TableHead>{t("accountInspection.result")}</TableHead><TableHead>HTTP</TableHead><TableHead>{t("accountInspection.evidence")}</TableHead><TableHead>{t("accountInspection.suggestion")}</TableHead><TableHead>{t("accountInspection.checkedAt")}</TableHead></TableRow></TableHeader>
          <TableBody>
            {detail.items.map((item) => (
              <TableRow key={item.accountId}>
                <TableCell><div className="max-w-52 truncate text-xs font-medium">{item.accountName}</div><div className="max-w-52 truncate text-[11px] text-muted-foreground">{item.accountEmail || item.accountId}</div></TableCell>
                <TableCell><Badge variant={classificationVariant(item.classification)}>{t(`accountInspection.classification.${item.classification}`)}</Badge></TableCell>
                <TableCell className="text-xs tabular-nums">{item.httpStatus || "-"}</TableCell>
                <TableCell><div className="max-w-72 truncate text-xs">{item.errorCode || item.failureScope || "-"}</div><div className="max-w-72 truncate text-[11px] text-muted-foreground">{item.errorMessage || `${item.latencyMilliseconds} ms`}</div></TableCell>
                <TableCell>
                  <div className="flex items-center gap-1.5 text-xs">{item.applyStatus === "applied" ? <CheckCircle2 className="size-3.5 text-emerald-600" /> : null}{t(`accountInspection.actionName.${item.suggestedAction}`)}</div>
                  <div className={cn("text-[11px] text-muted-foreground", item.applyStatus === "failed" && "text-destructive")}>{t(`accountInspection.applyState.${item.applyStatus}`)}</div>
                  {item.applyError ? <div className={cn("max-w-36 truncate text-[11px] text-muted-foreground", item.applyStatus === "failed" && "text-destructive")} title={item.applyStatus === "failed" ? t("accountInspection.applyReason.unknown") : undefined}>{applyReasonLabel(t, item.applyError)}</div> : null}
                </TableCell>
                <TableCell className="whitespace-nowrap text-xs text-muted-foreground">{formatDateTime(item.updatedAt, i18n.language)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

function applyReasonLabel(t: (key: string) => string, reason: string): string {
  switch (reason) {
    case "confidence_not_high": return t("accountInspection.applyReason.confidence_not_high");
    case "action_not_automatic": return t("accountInspection.applyReason.action_not_automatic");
    case "stale_evidence": return t("accountInspection.applyReason.stale_evidence");
    case "inspection_cancelled": return t("accountInspection.applyReason.inspection_cancelled");
    case "terminal_result_not_auto_applied": return t("accountInspection.applyReason.terminal_result_not_auto_applied");
    default: return t("accountInspection.applyReason.unknown");
  }
}

function RunStatusBadge({ run }: { run: AccountInspectionRunDTO }) {
  const { t } = useTranslation();
  return <Badge variant={run.status === "failed" ? "destructive" : run.status === "completed" ? "default" : "secondary"}>{t(`accountInspection.status.${run.status}`)}</Badge>;
}

function classificationVariant(classification: string): "default" | "secondary" | "destructive" | "outline" {
  if (classification === "healthy") return "default";
  if (classification === "reauth" || classification === "permission_denied" || classification === "quota_exhausted") return "destructive";
  if (classification === "probe_error" || classification === "model_unavailable") return "secondary";
  return "outline";
}
