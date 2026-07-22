import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Activity, CheckCircle2, CircleAlert, Clock3, Mail, Play, Plus, RefreshCw, RotateCcw, Save, Square, Terminal, Trash2, XCircle } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { getOperations, triggerReplenishment, type OperationsDTO } from "@/features/dashboard/operations-api";
import { ErrorState } from "@/shared/components/data-state";
import { formatDateTime, formatDuration, formatNumber } from "@/shared/lib/format";
import {
  getRegistrationLogs,
  getRegistrationPreflight,
  getRegistrationSettings,
  getRegistrationStatus,
  startRegistration,
  stopRegistration,
  updateRegistrationSettings,
  type EmailSourceDTO,
  type RegistrationSettingsDTO,
  type RegistrationStartInput,
} from "@/features/registration/registration-api";
import { listEgressGroups } from "@/features/settings/settings-api";

export function RegistrationPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const locale = i18n.language === "zh-CN" ? "zh-CN" : "en-US";
  const [draft, setDraft] = useState<RegistrationSettingsDTO | null>(null);
  const [settingsDirty, setSettingsDirty] = useState(false);
  const [stopOpen, setStopOpen] = useState(false);
  const [startInput, setStartInput] = useState<RegistrationStartInput>({ count: 0, extra: 1, threads: 1, fast: true, accountType: "build", autoNSFW: false });

  const statusQuery = useQuery({
    queryKey: ["registration", "status"],
    queryFn: ({ signal }) => getRegistrationStatus(signal),
    refetchInterval: (query) => query.state.data?.running ? 1500 : 5000,
    refetchIntervalInBackground: true,
    refetchOnMount: "always",
    refetchOnWindowFocus: "always",
  });
  const logsQuery = useQuery({
    queryKey: ["registration", "logs"],
    queryFn: ({ signal }) => getRegistrationLogs(500, signal),
    enabled: Boolean(statusQuery.data?.configured),
    refetchInterval: () => statusQuery.data?.running ? 1500 : 5000,
    refetchIntervalInBackground: true,
    refetchOnMount: "always",
    refetchOnWindowFocus: "always",
  });
  const settingsQuery = useQuery({
    queryKey: ["registration", "settings"],
    queryFn: ({ signal }) => getRegistrationSettings(signal),
    enabled: Boolean(statusQuery.data?.configured),
  });
  const proxyGroupsQuery = useQuery({ queryKey: ["egress-groups", "registration"], queryFn: () => listEgressGroups() });
  const replenishmentQuery = useQuery({
    queryKey: ["operations"],
    queryFn: getOperations,
    enabled: Boolean(statusQuery.data?.configured),
    refetchInterval: 15_000,
  });
  const preflightMutation = useMutation({ mutationFn: getRegistrationPreflight, onError: showError });
  useEffect(() => {
    if (preflightMutation.data) {
      void queryClient.invalidateQueries({ queryKey: ["registration", "logs"] });
    }
  }, [preflightMutation.data, queryClient]);
  const saveMutation = useMutation({
    mutationFn: (value: RegistrationSettingsDTO) => updateRegistrationSettings(value),
    onSuccess: (value) => {
      setDraft(value);
      setSettingsDirty(false);
      void queryClient.invalidateQueries({ queryKey: ["registration", "settings"] });
      toast.success(t("registration.saved"));
    },
    onError: showError,
  });
  const startMutation = useMutation({
    mutationFn: startRegistration,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["registration"] });
      toast.success(t("registration.started"));
    },
    onError: showError,
  });
  const stopMutation = useMutation({
    mutationFn: stopRegistration,
    onSuccess: () => {
      setStopOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["registration"] });
      toast.success(t("registration.stopped"));
    },
    onError: showError,
  });
  const replenishmentTrigger = useMutation({
    mutationFn: triggerReplenishment,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["operations"] });
      void queryClient.invalidateQueries({ queryKey: ["registration"] });
      toast.success(t("dashboardOperations.replenishmentRequested"));
    },
    onError: showError,
  });

  function showError(error: unknown): void {
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
  }

  function updateDraft<K extends keyof RegistrationSettingsDTO>(key: K, value: RegistrationSettingsDTO[K]): void {
    setDraft((current) => {
      const base = current ?? settingsQuery.data;
      return base ? { ...base, [key]: value } : current;
    });
    setSettingsDirty(true);
  }

  function updateEmailSource(id: string, patch: Partial<EmailSourceDTO>): void {
    if (!settings) return;
    updateDraft("emailSources", settings.emailSources.map((source) => source.id === id ? { ...source, ...patch } : source));
  }

  function addEmailSource(): void {
    if (!settings || settings.emailSources.length >= 2) return;
    if (settings.emailSources.some((source) => source.type === "tempmail_lol")) return;
    const type: EmailSourceDTO["type"] = "tempmail_lol";
    const nextID = Array.from({ length: 10 }, (_, index) => `source-new-${index + 1}`).find((id) => !settings.emailSources.some((source) => source.id === id)) ?? "source-new";
    const source: EmailSourceDTO = {
      id: nextID,
      type,
      enabled: true,
      apiBase: type === "tempmail_lol" ? "https://api.tempmail.lol" : "https://maliapi.215.im/v1",
      apiKey: "",
      jwt: "",
      domain: "",
      prefix: type === "tempmail_lol" ? "xai" : "",
      apiKeyConfigured: false,
      jwtConfigured: false,
    };
    updateDraft("emailSources", [...settings.emailSources, source]);
  }

  function removeEmailSource(id: string): void {
    if (!settings || settings.emailSources.length <= 1) return;
    const next = settings.emailSources.filter((source) => source.id !== id);
    if (!next.some((source) => source.enabled)) next[0] = { ...next[0], enabled: true };
    updateDraft("emailSources", next);
  }

  function updateStart<K extends keyof RegistrationStartInput>(key: K, value: RegistrationStartInput[K]): void {
    setStartInput((current) => ({ ...current, [key]: value }));
  }

  const status = statusQuery.data;
  const settings = draft ?? settingsQuery.data ?? null;
  const enabledEmailSourceCount = settings?.emailSources.filter((source) => source.enabled).length ?? 0;
  const progress = status?.progress;
  const progressPercent = progress?.percent ?? 0;
  const attempted = progress?.attempted ?? 0;
  const succeeded = progress?.succeeded ?? 0;
  const failed = progress?.failed ?? 0;
  const successRate = attempted > 0 ? Math.min(100, (succeeded / attempted) * 100) : null;
  const running = status?.running ?? false;
  const busy = running || startMutation.isPending || stopMutation.isPending;
  const displayError = status?.lastError;
  const logItems = logsQuery.data?.items ?? [];
  const taskDurationLabelText = locale === "zh-CN" ? "任务耗时" : "Task duration";
  const taskDurationDetailText = locale === "zh-CN"
    ? (running ? "运行中实时更新" : "本次任务总耗时")
    : (running ? "Live elapsed time" : "Total duration for this run");
  const averagePerAccountLabelText = locale === "zh-CN" ? "平均每号耗时" : "Avg per account";
  const averagePerAccountDetailText = locale === "zh-CN" ? "按已完成账号计算" : "Calculated from completed accounts";
  const durationLabel = status?.durationMs != null ? formatDuration(status.durationMs) : "-";
  const averagePerAccountLabel = status?.averagePerAccountMs != null ? formatDuration(status.averagePerAccountMs) : "-";
  const formattedProgress = useMemo(() => {
    if (!progress) return t("registration.noProgress");
    if (progress.indeterminate) return t("registration.progressUnlimited", { done: formatNumber(progress.done, locale) });
    return t("registration.progressFinite", { done: formatNumber(progress.done, locale), total: formatNumber(progress.total ?? 0, locale) });
  }, [locale, progress, t]);

  if (statusQuery.isError || settingsQuery.isError) {
    const error = statusQuery.error ?? settingsQuery.error;
    return <ErrorState message={error instanceof Error ? error.message : t("errors.generic")} onRetry={() => { void statusQuery.refetch(); void settingsQuery.refetch(); }} />;
  }

  if (status && !status.configured) {
    return (
      <div className="flex min-h-[calc(100vh-10rem)] items-center justify-center px-6">
        <div className="max-w-md space-y-2 text-center">
          <h1 className="text-xl font-medium">{t("registration.title")}</h1>
          <p className="text-sm text-muted-foreground">{t("registration.disabled")}</p>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-8">
      <header>
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <Terminal className="size-5 text-muted-foreground" strokeWidth={1.8} />
            <h1 className="text-xl font-medium">{t("registration.title")}</h1>
          </div>
          <p className="sr-only">{t("registration.description")}</p>
        </div>
      </header>

      <section className="border-y py-5">
        <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
          <div className="flex flex-wrap items-center gap-x-5 gap-y-3 text-xs">
            <StatusBadge running={running} error={Boolean(displayError)} label={statusLabel(status, t)} />
            {status?.pid ? <span className="text-muted-foreground">PID <span className="font-mono text-foreground">{status.pid}</span></span> : null}
            <span className="text-muted-foreground">{t("registration.accounts")}: <span className="text-foreground">{formatNumber(progress?.accountCount ?? 0, locale)}</span></span>
            {status?.startedAt ? <span className="text-muted-foreground">{t("registration.startedAt")}: <span className="text-foreground">{formatDateTime(status.startedAt, locale)}</span></span> : null}
            {status?.finishedAt ? <span className="text-muted-foreground">{t("registration.finishedAt")}: <span className="text-foreground">{formatDateTime(status.finishedAt, locale)}</span></span> : null}
          </div>
          <div className="flex shrink-0 flex-wrap items-center gap-2">
            <ReplenishmentControl
              value={replenishmentQuery.data?.replenishment}
              locale={locale}
              triggering={replenishmentTrigger.isPending}
              registrationRunning={running}
              onTrigger={() => replenishmentTrigger.mutate()}
            />
            <Button variant="secondary" size="sm" disabled={preflightMutation.isPending || busy || settingsDirty || !status?.configured} onClick={() => { preflightMutation.reset(); preflightMutation.mutate(); }}>
              {preflightMutation.isPending ? <Spinner /> : <RefreshCw />}{t("registration.preflight")}
            </Button>
            {running ? (
              <Button variant="destructive" size="sm" disabled={stopMutation.isPending} onClick={() => setStopOpen(true)}>
                {stopMutation.isPending ? <Spinner /> : <Square />}{t("registration.stop")}
              </Button>
            ) : (
              <Button size="sm" disabled={busy || settingsDirty || !status?.configured || startMutation.isPending} onClick={() => startMutation.mutate(startInput)}>
                {startMutation.isPending ? <Spinner /> : <Play />}{t("registration.start")}
              </Button>
            )}
          </div>
        </div>
        <div className="mt-5 grid border-y sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-6">
          <TaskMetric
            label={t("registration.taskCompletion")}
            value={progress?.indeterminate
              ? formatNumber(progress.done, locale)
              : `${formatNumber(progress?.done ?? 0, locale)} / ${formatNumber(progress?.total ?? 0, locale)}`}
            detail={progress?.percent == null ? t("registration.inProgressUnknown") : `${progress.percent.toFixed(1)}%`}
          />
          <TaskMetric label={taskDurationLabelText} value={durationLabel} detail={taskDurationDetailText} />
          <TaskMetric label={averagePerAccountLabelText} value={averagePerAccountLabel} detail={averagePerAccountDetailText} />
          <TaskMetric label={t("registration.succeeded")} value={formatNumber(succeeded, locale)} detail={t("registration.usableCredentials")} tone="success" />
          <TaskMetric label={t("registration.failedAttempts")} value={formatNumber(failed, locale)} detail={t("registration.attemptedDetail", { count: formatNumber(attempted, locale) })} tone={failed > 0 ? "danger" : "default"} />
          <TaskMetric label={t("registration.successRate")} value={successRate == null ? "-" : `${successRate.toFixed(1)}%`} detail={t("registration.successRateDetail")} />
        </div>
        <div className="mt-5 space-y-2">
          <div className="flex items-center justify-between gap-3 text-xs">
            <span className="text-muted-foreground">{t("registration.progress")}</span>
            <span className="font-mono text-foreground">{formattedProgress}</span>
          </div>
          <div className="h-1.5 overflow-hidden rounded-full bg-secondary">
            <div className="h-full rounded-full bg-primary transition-[width] duration-500" style={{ width: `${progress?.indeterminate ? 35 : progressPercent}%` }} />
          </div>
        </div>
        {(progress?.resumable ?? 0) > 0 ? (
          <div className="mt-4 flex items-center gap-2 text-xs text-amber-700 dark:text-amber-400">
            <RotateCcw className="size-4 shrink-0" />
            <span>{t("registration.resumableDetail", { count: formatNumber(progress?.resumable ?? 0, locale) })}</span>
          </div>
        ) : null}
        {displayError ? (
          <div className="mt-4 flex items-start gap-2 text-xs text-destructive">
            <CircleAlert className="mt-0.5 size-4 shrink-0" />
            <span>{displayError.message}</span>
          </div>
        ) : null}
        {preflightMutation.data ? (
          <section className="mt-5 border-t pt-4">
            <div className="flex items-center gap-2">
              {preflightMutation.data.ok ? <CheckCircle2 className="size-4 text-emerald-600 dark:text-emerald-400" /> : <CircleAlert className="size-4 text-destructive" />}
              <h2 className="text-sm font-medium">{t("registration.preflightResult")}</h2>
              <Badge variant={preflightMutation.data.ok ? "secondary" : "destructive"}>
                {preflightMutation.data.ok ? t("registration.succeeded") : t("registration.failed")}
              </Badge>
            </div>
            <div className="mt-3 grid gap-2 sm:grid-cols-2 xl:grid-cols-3">
              {preflightMutation.data.checks.map((check) => (
                <div key={check.name} className={`min-w-0 border p-2 ${check.ok ? "border-emerald-200/70 dark:border-emerald-900/70" : "border-destructive/40 bg-destructive/5"}`}>
                  <div className="flex min-w-0 items-start gap-2">
                    {check.ok ? <CheckCircle2 className="mt-0.5 size-3.5 shrink-0 text-emerald-600 dark:text-emerald-400" /> : <CircleAlert className="mt-0.5 size-3.5 shrink-0 text-destructive" />}
                    <div className="min-w-0">
                      <div className="truncate text-xs font-medium">{preflightCheckLabel(check.name, locale, t)}</div>
                      <div className="mt-1 break-words text-[11px] leading-4 text-muted-foreground">{check.detail || "-"}</div>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          </section>
        ) : null}
      </section>

      <div className="grid items-start gap-10 xl:grid-cols-[minmax(0,5fr)_minmax(420px,7fr)]">
        <div className="space-y-9">
          <section className="space-y-5">
            <SectionHeading title={t("registration.taskTitle")} description={t("registration.taskDescription")} />
            <div className="grid gap-4 sm:grid-cols-2">
              <SelectField
                label={t("registration.accountType")}
                value={startInput.accountType}
                disabled={busy}
                options={[
                  { value: "web", label: t("registration.accountTypeWeb") },
                  { value: "build", label: t("registration.accountTypeBuild") },
                ]}
                onChange={(value) => {
                  const accountType = value as RegistrationStartInput["accountType"];
                  setStartInput((current) => ({ ...current, accountType, autoNSFW: accountType === "web" ? true : false }));
                }}
              />
              <NumberField
                label={t(running ? "registration.nextBatchCount" : "registration.batchCount")}
                value={startInput.extra}
                min={1}
                max={10000}
                disabled={startMutation.isPending}
                onChange={(value) => updateStart("extra", value)}
              />
              <NumberField label={t("registration.threads")} value={startInput.threads} min={1} max={10} disabled={busy} onChange={(value) => updateStart("threads", value)} />
              <ToggleField label={t("registration.fast")} description={t("registration.fastDescription")} checked={startInput.fast} disabled={busy} onCheckedChange={(value) => updateStart("fast", value)} />
              {startInput.accountType === "web" ? <ToggleField label={t("registration.autoNSFW")} description={t("registration.autoNSFWDescription")} checked={startInput.autoNSFW} disabled={busy} onCheckedChange={(value) => updateStart("autoNSFW", value)} /> : null}
            </div>
          </section>

          <section className="space-y-5 border-t pt-8">
            <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
              <SectionHeading title={t("registration.emailTitle")} description={t("registration.emailDescription")} />
              <div className="flex shrink-0 items-center gap-2">
                {settingsDirty ? (
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={busy || saveMutation.isPending}
                    onClick={() => { setDraft(settingsQuery.data ?? null); setSettingsDirty(false); }}
                  >
                    <RotateCcw />{t("common.reset")}
                  </Button>
                ) : null}
                <Button variant="secondary" size="sm" disabled={!settings || busy || saveMutation.isPending || !settingsDirty} onClick={() => { if (settings) saveMutation.mutate(settings); }}>
                  {saveMutation.isPending ? <Spinner /> : <Save />}{t("common.save")}
                </Button>
              </div>
            </div>
            {settings ? (
              <div className="space-y-6">
                <div className="rounded-md border p-4">
                  <div className="flex flex-wrap items-center justify-between gap-3">
                    <div className="flex items-center gap-2">
                      <Mail className="size-4 text-muted-foreground" />
                      <h3 className="text-sm font-medium">{t("registration.emailSources")}</h3>
                    </div>
                    <div className="flex items-center gap-2">
                      <Badge variant="outline">{t("registration.emailSourcesEnabled", { enabled: enabledEmailSourceCount, total: settings.emailSources.length })}</Badge>
                      <Button variant="outline" size="sm" disabled={busy || settings.emailSources.length >= 2} onClick={addEmailSource}>
                        <Plus />{t("registration.addEmailSource")}
                      </Button>
                    </div>
                  </div>
                  <div className="mt-4 space-y-3">
                    {settings.emailSources.map((source, index) => (
                      <EmailSourceCard
                        key={source.id}
                        source={source}
                        index={index}
                        disabled={busy}
                        canDisable={!source.enabled || enabledEmailSourceCount > 1}
                        canDelete={settings.emailSources.length > 1}
                        usedTypes={settings.emailSources.filter((item) => item.id !== source.id).map((item) => item.type)}
                        onChange={(patch) => updateEmailSource(source.id, patch)}
                        onDelete={() => removeEmailSource(source.id)}
                      />
                    ))}
                  </div>
                </div>

                <div className="grid gap-4 border-t pt-5 sm:grid-cols-2">
                  <SelectField label={t("registration.engine")} value={settings.engine} disabled={busy} options={[
                    { value: "protocol", label: t("registration.engineProtocol") },
                    { value: "browser", label: t("registration.engineBrowser") },
                  ]} onChange={(value) => {
                    updateDraft("engine", value);
                  }} />
                  {settings.engine === "protocol" ? (
                    <>
                      <SelectField label={t("registration.captchaSolver")} value={settings.captchaSolver} disabled={busy} options={[
                        { value: "local", label: t("registration.captchaLocal") },
                        { value: "yescaptcha", label: t("registration.captchaYes") },
                      ]} onChange={(value) => updateDraft("captchaSolver", value)} />
                      {settings.captchaSolver === "local" ? (
                        <TextField label={t("registration.captchaEndpoint")} value={settings.captchaEndpoint} disabled={busy} placeholder="http://grok-turnstile-solver:5072" onChange={(value) => updateDraft("captchaEndpoint", value)} />
                      ) : (
                        <SecretField label={t("registration.yescaptchaApiKey")} value={settings.yescaptchaApiKey} configured={settings.yescaptchaApiKeyConfigured} disabled={busy} onChange={(value) => updateDraft("yescaptchaApiKey", value)} />
                      )}
                    </>
                  ) : null}
                  <SelectField label={t("registration.proxyGroup")} value={settings.proxyGroupId || "direct"} disabled={busy} options={[{ value: "direct", label: t("registration.direct") }, ...(proxyGroupsQuery.data?.items ?? []).filter((group) => group.enabled && group.scope === (startInput.accountType === "web" ? "grok_web" : "grok_build")).map((group) => ({ value: group.id, label: `${group.name} (${group.enabledMembers}/${group.memberCount})` }))]} onChange={(value) => updateDraft("proxyGroupId", value === "direct" ? "" : value)} />
                  <TextField label={t("registration.proxy")} value={settings.proxy} disabled={busy || Boolean(settings.proxyGroupId)} placeholder={settings.proxyGroupId ? t("registration.proxyFromGroup") : t("registration.direct")} onChange={(value) => updateDraft("proxy", value)} />
                </div>
              </div>
            ) : <Spinner />}
          </section>

          {startInput.accountType === "web" ? (
            <section className="space-y-2 border-t pt-8">
              <SectionHeading title={t("registration.webOutputTitle")} description={t("registration.webOutputDescription")} />
            </section>
          ) : null}

        </div>

        <section className="min-w-0 space-y-4 xl:sticky xl:top-6">
          <div className="flex items-center justify-between gap-3">
            <SectionHeading title={t("registration.logs")} description={t("registration.logsDescription")} />
            <Badge variant="outline">{logItems.length}</Badge>
          </div>
          <div className="max-h-[calc(100vh-13rem)] min-h-[28rem] overflow-y-auto border bg-muted/20 p-3">
            {logsQuery.isError ? <ErrorState message={logsQuery.error.message} onRetry={() => void logsQuery.refetch()} /> : null}
            {logsQuery.isPending ? <div className="flex min-h-40 items-center justify-center"><Spinner /></div> : null}
            {!logsQuery.isPending && logItems.length === 0 ? <div className="flex min-h-40 items-center justify-center text-xs text-muted-foreground">{t("registration.noLogs")}</div> : null}
            <div className="space-y-1 font-mono text-[11px] leading-5">
              {logItems.map((entry) => <div key={entry.id} className="break-words text-muted-foreground"><span className="mr-2 select-none text-foreground/35">{entry.id}</span>{renderRegistrationLog(entry.text, locale)}</div>)}
            </div>
          </div>
        </section>
      </div>

      <AlertDialog open={stopOpen} onOpenChange={setStopOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("registration.stopTitle")}</AlertDialogTitle>
            <AlertDialogDescription>{t("registration.stopDescription")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={stopMutation.isPending} onClick={() => stopMutation.mutate()}>{t("registration.stop")}</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function ReplenishmentControl({ value, locale, triggering, registrationRunning, onTrigger }: {
  value?: OperationsDTO["replenishment"];
  locale: string;
  triggering: boolean;
  registrationRunning: boolean;
  onTrigger: () => void;
}) {
  const { t } = useTranslation();
  if (!value) return <Spinner className="mx-2 size-4" />;
  if (!value.enabled) return <Badge variant="outline" className="text-muted-foreground">{t("dashboardOperations.replenishmentDisabled")}</Badge>;

  const active = value.state === "starting" || value.state === "running" || value.state === "verifying";
  return (
    <div className="flex flex-wrap items-center gap-2 text-[11px] text-muted-foreground">
      <Badge variant={value.state === "failed" ? "destructive" : "outline"} className="gap-1">
        <RefreshCw className={`size-3 ${active ? "animate-spin" : ""}`} />
        {t(`dashboardOperations.replenishment_${value.state}`)}
      </Badge>
      <span>{t("dashboardOperations.dailyStarts", { used: value.dailyStarts, limit: value.maxDailyRegistrations })}</span>
      {value.predictive ? <span>{t("dashboardOperations.predictiveThreshold", { eligible: value.targetEligible, rpm: formatNumber(value.minDemandRPM, locale, 1), minutes: formatNumber(value.demandWindowSeconds / 60, locale, 0) })}</span> : null}
      {value.nextAttemptAt ? <span>{formatDateTime(value.nextAttemptAt, locale)}</span> : null}
      <Button type="button" variant="secondary" size="sm" disabled={triggering || registrationRunning || active} onClick={onTrigger}>
        {triggering ? <Spinner /> : <RefreshCw />}{t("dashboardOperations.triggerReplenishment")}
      </Button>
    </div>
  );
}

function preflightCheckLabel(name: string, locale: string, t: (key: string) => string): string {
  const localLabels: Record<string, string> = locale === "zh-CN" ? {
    browserRuntime: "浏览器运行环境",
    browserWorker: "浏览器 Worker",
    browserModule: "浏览器注册模块",
    turnstileManifest: "Turnstile 清单",
    turnstileContent: "Turnstile 脚本",
    chromium: "Chrome/Chromium",
    display: "显示环境",
    browserProxyAuth: "浏览器代理认证",
    cpaBrowserProxyAuth: "CPA 浏览器代理认证",
    egressIP: "出口 IP",
    registrationPage: "注册页面",
    browserChromium: "Worker Chrome/Chromium",
    browserDisplay: "Worker 显示环境",
    browserRegistrationPage: "Worker 注册页面",
    grokRegisterImport: "注册模块导入",
    drissionPage: "DrissionPage",
    dependencies: "Worker 依赖",
  } : {
    browserRuntime: "Browser runtime",
    browserWorker: "Browser worker",
    browserModule: "Browser registration module",
    turnstileManifest: "Turnstile manifest",
    turnstileContent: "Turnstile script",
    chromium: "Chrome/Chromium",
    display: "Display environment",
    browserProxyAuth: "Browser proxy auth",
    cpaBrowserProxyAuth: "CPA browser proxy auth",
    egressIP: "Egress IP",
    registrationPage: "Registration page",
    browserChromium: "Worker Chrome/Chromium",
    browserDisplay: "Worker display",
    browserRegistrationPage: "Worker registration page",
    grokRegisterImport: "Registration module import",
    drissionPage: "DrissionPage",
    dependencies: "Worker dependencies",
  };
  if (localLabels[name]) return localLabels[name];
  if (name.startsWith("emailAPI:")) return `${t("registration.preflightChecks.emailAPI")} ${name.slice("emailAPI:".length)}`;
  if (name.startsWith("emailReachability:")) return `${locale === "zh-CN" ? "邮箱可达性" : "Email reachability"} ${name.slice("emailReachability:".length)}`;
  const translated = t(`registration.preflightChecks.${name}`);
  return translated === `registration.preflightChecks.${name}` ? name : translated;
}

function renderRegistrationLog(text: string, locale: string) {
  const value = localizeRegistrationLog(text, locale);
  const passed = /（通过）$/.test(value);
  const failed = /（失败）$/.test(value);
  if (!passed && !failed) return value;
  const detail = value.replace(/（(?:通过|失败)）$/, "");
  return <span className={passed ? "inline-flex items-center gap-1 text-emerald-600 dark:text-emerald-400" : "inline-flex items-center gap-1 text-destructive"}>
    {passed ? <CheckCircle2 className="size-3.5 shrink-0" /> : <XCircle className="size-3.5 shrink-0" />}
    <span>{detail}</span>
  </span>;
}

function EmailSourceCard({ source, index, disabled, canDisable, canDelete, usedTypes, onChange, onDelete }: {
  source: EmailSourceDTO;
  index: number;
  disabled: boolean;
  canDisable: boolean;
  canDelete: boolean;
  usedTypes: EmailSourceDTO["type"][];
  onChange: (patch: Partial<EmailSourceDTO>) => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const providerName = source.type === "tempmail_lol" ? "TempMail.lol" : "YYDS Mail";
  const available = source.type === "tempmail_lol" || Boolean(source.apiKey.trim() || source.apiKeyConfigured || source.jwt.trim() || source.jwtConfigured);

  function changeType(type: EmailSourceDTO["type"]): void {
    onChange({
      type,
      apiBase: type === "tempmail_lol" ? "https://api.tempmail.lol" : "https://maliapi.215.im/v1",
      apiKey: "",
      jwt: "",
      domain: "",
      prefix: type === "tempmail_lol" ? "xai" : "",
      apiKeyConfigured: false,
      jwtConfigured: false,
    });
  }

  return (
    <div className="rounded-md border bg-muted/20 p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <h4 className="text-sm font-semibold">{t("registration.emailSourceNumber", { number: index + 1 })}</h4>
          <Badge variant="outline" className="font-normal">{providerName}</Badge>
          <Badge variant={available ? "secondary" : "outline"} className={available ? "border-emerald-200 bg-emerald-50 text-emerald-700 dark:border-emerald-900 dark:bg-emerald-950 dark:text-emerald-300" : "text-muted-foreground"}>
            {t(available ? "registration.emailSourceAvailable" : "registration.emailSourceNeedsConfig")}
          </Badge>
        </div>
        <div className="flex items-center gap-3">
          <Label className="flex items-center gap-2 text-xs font-normal">
            <Checkbox
              checked={source.enabled}
              disabled={disabled || !canDisable}
              onCheckedChange={(checked) => onChange({ enabled: checked === true })}
            />
            {t("common.enable")}
          </Label>
          <Button variant="ghost" size="icon" disabled={disabled || !canDelete} onClick={onDelete} aria-label={t("registration.deleteEmailSource")} title={t("registration.deleteEmailSource")}>
            <Trash2 />
          </Button>
        </div>
      </div>

      <div className="my-4 flex items-center gap-3">
        <span className="text-[11px] text-muted-foreground">{t("registration.emailSourceBasic")}</span>
        <div className="h-px flex-1 bg-border" />
      </div>

      <div className="grid gap-4 sm:grid-cols-2">
        <div className="space-y-2">
          <Label className="text-xs">{t("registration.emailSourceType")}</Label>
          <Select value={source.type} disabled={disabled} onValueChange={(value) => changeType(value as EmailSourceDTO["type"])}>
            <SelectTrigger><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="tempmail_lol" disabled={usedTypes.includes("tempmail_lol")}>TempMail.lol</SelectItem>
              <SelectItem value="yyds" disabled={usedTypes.includes("yyds")}>YYDS Mail</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <TextField label={t("registration.emailSourceAPIBase")} value={source.apiBase} disabled={disabled} onChange={(value) => onChange({ apiBase: value })} />
        <SecretField label={t("registration.emailSourceAPIKey")} value={source.apiKey} configured={source.apiKeyConfigured} disabled={disabled} onChange={(value) => onChange({ apiKey: value })} />
        {source.type === "yyds" ? (
          <SecretField label={t("registration.yydsJwt")} value={source.jwt} configured={source.jwtConfigured} disabled={disabled} onChange={(value) => onChange({ jwt: value })} />
        ) : (
          <TextField label={t("registration.tempmailPrefix")} value={source.prefix} disabled={disabled} placeholder="xai" onChange={(value) => onChange({ prefix: value })} />
        )}
        {source.type === "tempmail_lol" ? (
          <div className="space-y-2 sm:col-span-2">
            <Label className="text-xs">{t("registration.emailSourceDomains")}</Label>
            <Textarea value={source.domain} disabled={disabled} placeholder={t("registration.emailSourceDomainsPlaceholder")} onChange={(event) => onChange({ domain: event.target.value })} />
          </div>
        ) : null}
      </div>
    </div>
  );
}

function SectionHeading({ title, description }: { title: string; description: string }) {
  return <div><h2 className="text-sm font-medium">{title}</h2><p className="mt-1 text-xs text-muted-foreground">{description}</p></div>;
}

function StatusBadge({ running, error, label }: { running: boolean; error: boolean; label: string }) {
  return <Badge variant={error ? "destructive" : running ? "default" : "secondary"}>{running ? <Activity className="mr-1 size-3" /> : error ? <CircleAlert className="mr-1 size-3" /> : <Clock3 className="mr-1 size-3" />}{label}</Badge>;
}

function TaskMetric({ label, value, detail, tone = "default" }: { label: string; value: string; detail: string; tone?: "default" | "success" | "danger" }) {
  const valueClass = tone === "success" ? "text-emerald-700 dark:text-emerald-400" : tone === "danger" ? "text-destructive" : "text-foreground";
  return (
    <div className="min-w-0 px-4 py-4 first:pl-0 sm:border-r sm:[&:nth-child(2)]:border-r-0 xl:[&:nth-child(2)]:border-r xl:last:border-r-0">
      <p className="text-[11px] text-muted-foreground">{label}</p>
      <p className={`mt-1 font-mono text-xl font-medium ${valueClass}`}>{value}</p>
      <p className="mt-1 truncate text-[11px] text-muted-foreground" title={detail}>{detail}</p>
    </div>
  );
}

function statusLabel(status: { running: boolean; lastError?: { code: string } } | undefined, t: (key: string) => string): string {
  if (!status) return t("common.loading");
  if (status.running) return t("registration.running");
  if (status.lastError?.code === "registrationPartial") return t("registration.partial");
  if (status.lastError) return t("registration.failed");
  return t("registration.idle");
}

function localizeRegistrationLog(text: string, locale: string): string {
  if (locale !== "zh-CN") return text;
  let value = text
    .replace(/\[protocol\]/gi, "[协议]")
    .replace(/\[website\]/gi, "[管理端]")
    .replace(/\[cpa\]/gi, "[CPA]");
  value = value
    .replace(/post-inject session url=(\S+)\s+visible=.*$/i, "注入登录状态后的页面：$1")
    .replace(/\bvisible:.*$/i, "页面内容已更新")
    .replace(/open device url:\s*/i, "正在打开设备授权页：")
    .replace(/stop_event set\s*[—-]\s*leave browser loop/i, "令牌已获取，结束浏览器授权流程")
    .replace(/token poll SUCCESS\s*[—-]\s*stop_event set/i, "令牌获取成功，正在结束授权流程")
    .replace(/device done page\s*[—-]\s*waiting for token poll/i, "设备授权已完成，正在等待令牌")
    .replace(/oauth poll:\s*authorization_pending\s*\(sleep\s*(\d+(?:\.\d+)?)s\)/i, "OAuth 等待授权，$1 秒后重试")
    .replace(/oauth poll:\s*slow_down\s*\(sleep\s*(\d+(?:\.\d+)?)s\)/i, "OAuth 轮询过快，$1 秒后重试")
    .replace(/clicked REAL exact\s*/i, "已点击按钮：")
    .replace(/clicked JS exact\s*/i, "已通过脚本点击按钮：")
    .replace(/\burl:\s*/i, "页面已切换：")
    .replace(/\bwrote\s+/i, "CPA 凭据已写入：")
    .replace(/browser finished via stop_event/i, "浏览器授权流程已结束");
  return value;
}

function NumberField({ label, value, min, max, disabled, onChange }: { label: string; value: number; min: number; max: number; disabled: boolean; onChange: (value: number) => void }) {
  return <div className="space-y-2"><Label className="text-xs">{label}</Label><Input type="number" min={min} max={max} value={value} disabled={disabled} onChange={(event) => onChange(Math.max(min, Math.min(max, Number(event.target.value) || 0)))} /></div>;
}

function TextField({ label, value, disabled, placeholder, className, onChange }: { label: string; value: string; disabled: boolean; placeholder?: string; className?: string; onChange: (value: string) => void }) {
  return <div className={`space-y-2 ${className ?? ""}`}><Label className="text-xs">{label}</Label><Input value={value} disabled={disabled} placeholder={placeholder} onChange={(event) => onChange(event.target.value)} /></div>;
}

function SecretField({ label, value, configured, disabled, onChange }: { label: string; value: string; configured: boolean; disabled: boolean; onChange: (value: string) => void }) {
  const { t } = useTranslation();
  return <div className="space-y-2"><Label className="text-xs">{label}</Label><Input type="password" autoComplete="off" value={value} disabled={disabled} placeholder={configured ? t("registration.secretConfigured") : t("registration.secretMissing")} onChange={(event) => onChange(event.target.value)} /></div>;
}

function SelectField({ label, value, disabled, options, onChange }: { label: string; value: string; disabled: boolean; options: Array<{ value: string; label: string }>; onChange: (value: string) => void }) {
  return <div className="space-y-2"><Label className="text-xs">{label}</Label><select className="h-8 w-full rounded-md border border-input bg-background px-2 text-xs outline-none focus:ring-1 focus:ring-ring disabled:cursor-not-allowed disabled:opacity-50" value={value} disabled={disabled} onChange={(event) => onChange(event.target.value)}>{options.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select></div>;
}

function ToggleField({ label, description, checked, disabled, onCheckedChange }: { label: string; description?: string; checked: boolean; disabled: boolean; onCheckedChange: (checked: boolean) => void }) {
  return <div className="flex items-center justify-between gap-3"><div className="min-w-0"><Label className="text-xs">{label}</Label>{description ? <p className="mt-1 text-[11px] text-muted-foreground">{description}</p> : null}</div><Switch checked={checked} disabled={disabled} onCheckedChange={onCheckedChange} aria-label={label} /></div>;
}
