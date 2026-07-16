import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Activity, CheckCircle2, CircleAlert, Clock3, Play, RefreshCw, RotateCcw, Save, Square, Terminal, XCircle } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { ErrorState } from "@/shared/components/data-state";
import { formatDateTime, formatNumber } from "@/shared/lib/format";
import {
  getRegistrationLogs,
  getRegistrationPreflight,
  getRegistrationSettings,
  getRegistrationStatus,
  startRegistration,
  stopRegistration,
  updateRegistrationSettings,
  type RegistrationSettingsDTO,
  type RegistrationStartInput,
} from "@/features/registration/registration-api";

const providers = ["tempmail_lol", "duckmail", "yyds", "cloudflare", "cloudmail"] as const;

export function RegistrationPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const locale = i18n.language === "zh-CN" ? "zh-CN" : "en-US";
  const [draft, setDraft] = useState<RegistrationSettingsDTO | null>(null);
  const [settingsDirty, setSettingsDirty] = useState(false);
  const [stopOpen, setStopOpen] = useState(false);
  const [startInput, setStartInput] = useState<RegistrationStartInput>({ count: 0, extra: 1, threads: 1, fast: true, accountType: "build" });

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
  const preflightMutation = useMutation({ mutationFn: getRegistrationPreflight });
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

  function updateStart<K extends keyof RegistrationStartInput>(key: K, value: RegistrationStartInput[K]): void {
    setStartInput((current) => ({ ...current, [key]: value }));
  }

  const status = statusQuery.data;
  const settings = draft ?? settingsQuery.data ?? null;
  const progress = status?.progress;
  const progressPercent = progress?.percent ?? 0;
  const attempted = progress?.attempted ?? 0;
  const succeeded = progress?.succeeded ?? 0;
  const failed = progress?.failed ?? 0;
  const successRate = attempted > 0 ? Math.min(100, (succeeded / attempted) * 100) : null;
  const running = status?.running ?? false;
  const busy = running || startMutation.isPending || stopMutation.isPending;
  const checkResult = preflightMutation.data;
  const displayError = status?.lastError;
  const logItems = logsQuery.data?.items ?? [];
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
            <Button variant="secondary" size="sm" disabled={preflightMutation.isPending || busy || settingsDirty || !status?.configured} onClick={() => preflightMutation.mutate()}>
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
        <div className="mt-5 grid border-y sm:grid-cols-2 xl:grid-cols-4">
          <TaskMetric
            label={t("registration.taskCompletion")}
            value={progress?.indeterminate
              ? formatNumber(progress.done, locale)
              : `${formatNumber(progress?.done ?? 0, locale)} / ${formatNumber(progress?.total ?? 0, locale)}`}
            detail={progress?.percent == null ? t("registration.inProgressUnknown") : `${progress.percent.toFixed(1)}%`}
          />
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
                onChange={(value) => updateStart("accountType", value as RegistrationStartInput["accountType"])}
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
              <div className="grid gap-4 sm:grid-cols-2">
                <SelectField label={t("registration.engine")} value={settings.engine} disabled={busy} options={[
                  { value: "browser", label: t("registration.engineBrowser") },
                  { value: "protocol", label: t("registration.engineProtocol") },
                ]} onChange={(value) => updateDraft("engine", value)} />
                <SelectField label={t("registration.emailProvider")} value={settings.emailProvider} disabled={busy} options={providers.map((value) => ({ value, label: value }))} onChange={(value) => updateDraft("emailProvider", value)} />
                <TextField label={t("registration.fallbacks")} value={settings.emailProviderFallbacks.join(", ")} disabled={busy} placeholder="yyds, cloudmail" onChange={(value) => updateDraft("emailProviderFallbacks", value.split(",").map((item) => item.trim()).filter(Boolean))} />
                <TextField label={t("registration.tempmailAPI")} value={settings.tempmailLolApiBase} disabled={busy} onChange={(value) => updateDraft("tempmailLolApiBase", value)} />
                <TextField label={t("registration.tempmailDomain")} value={settings.tempmailLolDomain} disabled={busy} placeholder={t("registration.optional")} onChange={(value) => updateDraft("tempmailLolDomain", value)} />
                <TextField label={t("registration.tempmailPrefix")} value={settings.tempmailLolPrefix} disabled={busy} placeholder={t("registration.optional")} onChange={(value) => updateDraft("tempmailLolPrefix", value)} />
                <TextField label={t("registration.proxy")} value={settings.proxy} disabled={busy} placeholder={t("registration.direct")} onChange={(value) => updateDraft("proxy", value)} />
              </div>
            ) : <Spinner />}
          </section>

          {startInput.accountType === "build" ? <section className="space-y-5 border-t pt-8">
            <SectionHeading title={t("registration.cpaTitle")} description={t("registration.cpaDescription")} />
            {settings ? (
              <div className="grid gap-4 sm:grid-cols-2">
                <TextField className="sm:col-span-2" label={t("registration.cpaBaseURL")} value={settings.cpaBaseURL} disabled={busy} onChange={(value) => updateDraft("cpaBaseURL", value)} />
                <TextField label={t("registration.cpaProxy")} value={settings.cpaProxy} disabled={busy} placeholder={t("registration.sameProxy")} onChange={(value) => updateDraft("cpaProxy", value)} />
                <div className="sm:col-span-2 grid gap-3 border-t pt-4 sm:grid-cols-3">
                  <ToggleField label={t("registration.cpaProbeChat")} checked={settings.cpaProbeChat} disabled={busy} onCheckedChange={(value) => updateDraft("cpaProbeChat", value)} />
                  <ToggleField label={t("registration.cpaCloseBrowser")} checked={settings.cpaCloseBrowserAfterAuth} disabled={busy} onCheckedChange={(value) => updateDraft("cpaCloseBrowserAfterAuth", value)} />
                  <ToggleField label={t("registration.cpaHeadless")} checked={settings.cpaHeadless} disabled={busy} onCheckedChange={(value) => updateDraft("cpaHeadless", value)} />
                </div>
              </div>
            ) : <Spinner />}
          </section> : (
            <section className="space-y-2 border-t pt-8">
              <SectionHeading title={t("registration.webOutputTitle")} description={t("registration.webOutputDescription")} />
            </section>
          )}

          {checkResult ? (
            <section className="space-y-4 border-t pt-8">
              <div className="flex items-center gap-2">
                {checkResult.ok ? <CheckCircle2 className="size-4 text-emerald-600" /> : <XCircle className="size-4 text-destructive" />}
                <h2 className="text-sm font-medium">{t("registration.preflightResult")}</h2>
              </div>
              <div className="divide-y border-y text-xs">
                {checkResult.checks.map((check) => (
                  <div key={check.name} className="grid gap-2 py-2.5 sm:grid-cols-[160px_minmax(0,1fr)]">
                    <span className={check.ok ? "text-foreground" : "text-destructive"}>{check.name}</span>
                    <span className="break-all text-muted-foreground">{check.detail}</span>
                  </div>
                ))}
              </div>
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
              {logItems.map((entry) => <div key={entry.id} className="break-words text-muted-foreground"><span className="mr-2 select-none text-foreground/35">{entry.id}</span>{localizeRegistrationLog(entry.text, locale)}</div>)}
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

function SelectField({ label, value, disabled, options, onChange }: { label: string; value: string; disabled: boolean; options: Array<{ value: string; label: string }>; onChange: (value: string) => void }) {
  return <div className="space-y-2"><Label className="text-xs">{label}</Label><select className="h-8 w-full rounded-md border border-input bg-background px-2 text-xs outline-none focus:ring-1 focus:ring-ring disabled:cursor-not-allowed disabled:opacity-50" value={value} disabled={disabled} onChange={(event) => onChange(event.target.value)}>{options.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select></div>;
}

function ToggleField({ label, description, checked, disabled, onCheckedChange }: { label: string; description?: string; checked: boolean; disabled: boolean; onCheckedChange: (checked: boolean) => void }) {
  return <div className="flex items-center justify-between gap-3"><div className="min-w-0"><Label className="text-xs">{label}</Label>{description ? <p className="mt-1 text-[11px] text-muted-foreground">{description}</p> : null}</div><Switch checked={checked} disabled={disabled} onCheckedChange={onCheckedChange} aria-label={label} /></div>;
}
