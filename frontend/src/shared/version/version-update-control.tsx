import { ExternalLink, RefreshCw } from "lucide-react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { cn } from "@/shared/lib/cn";
import { currentVersion, useVersionUpdate } from "@/shared/version/use-version-update";
import type { ReleaseEntryType } from "@/shared/version/version-manifest";

const categoryStyles: Record<ReleaseEntryType, string> = {
  feature: "border-emerald-500/30 bg-emerald-500/8 text-emerald-700 dark:text-emerald-300",
  fix: "border-rose-500/30 bg-rose-500/8 text-rose-700 dark:text-rose-300",
  security: "border-amber-500/30 bg-amber-500/8 text-amber-700 dark:text-amber-300",
  ops: "border-sky-500/30 bg-sky-500/8 text-sky-700 dark:text-sky-300",
  improvement: "border-violet-500/30 bg-violet-500/8 text-violet-700 dark:text-violet-300",
  style: "border-indigo-500/30 bg-indigo-500/8 text-indigo-700 dark:text-indigo-300",
};

export function VersionUpdateControl() {
  const { i18n } = useTranslation();
  const state = useVersionUpdate();
  const chinese = i18n.language.startsWith("zh");
  const copy = chinese ? zhCopy : enCopy;
  const releases = state.manifest?.releases ?? [];
  const repositoryURL = state.manifest?.repositoryURL ?? "https://github.com/owen891/grok2api";

  return (
    <>
      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="fixed right-14 top-2 z-40 h-8 px-2 text-[11px] font-normal text-muted-foreground lg:left-[194px] lg:right-auto lg:top-6 lg:h-7 lg:px-2"
        onClick={() => state.setOpen(true)}
        aria-label={copy.open}
      >
        <span className="relative">
          {currentVersion}
          {state.updateAvailable ? <span className="absolute -right-2 -top-1 size-1.5 rounded-full bg-emerald-500" aria-label={copy.updateAvailable} /> : null}
        </span>
      </Button>

      <Dialog open={state.open} onOpenChange={state.setOpen}>
        <DialogContent className="flex max-h-[calc(100vh-2rem)] max-w-[760px] flex-col gap-0 overflow-hidden p-0 sm:max-h-[min(760px,calc(100vh-2rem))]">
          <DialogHeader className="border-b px-5 py-5 pr-12 text-left">
            <DialogTitle className="text-lg">{copy.title}</DialogTitle>
            <DialogDescription className="sr-only">{copy.description}</DialogDescription>
          </DialogHeader>

          <div className="grid shrink-0 gap-3 px-5 py-4 sm:grid-cols-2">
            <VersionPanel label={copy.currentVersion} version={currentVersion} />
            <VersionPanel
              label={copy.latestVersion}
              version={state.latestVersion}
              action={(
                <button
                  type="button"
                  className="inline-flex items-center gap-1 text-[11px] text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
                  disabled={state.checking}
                  onClick={() => void state.check()}
                >
                  <RefreshCw className={cn("size-3", state.checking && "animate-spin")} />
                  {state.checking ? copy.checking : copy.check}
                </button>
              )}
            />
          </div>

          <div className="min-h-0 flex-1 overflow-y-auto px-5 pb-4">
            {state.checkFailed && releases.length === 0 ? (
              <div className="flex min-h-40 items-center justify-center text-sm text-muted-foreground">{copy.checkFailed}</div>
            ) : null}
            <div className="space-y-5 border-l pl-5">
              {releases.map((release) => {
                const latest = release.version === state.latestVersion;
                const current = release.version === currentVersion;
                return (
                  <section key={release.version} className="relative">
                    <span className="absolute -left-[24.5px] top-1.5 size-2 rounded-full border bg-background" />
                    <div className="mb-3 flex flex-wrap items-center gap-2">
                      <h3 className="text-sm font-semibold">{release.version}</h3>
                      <span className="text-xs text-muted-foreground">{release.date}</span>
                      {latest ? <Badge className="border-emerald-500/30 bg-emerald-500/8 text-emerald-700 dark:text-emerald-300" variant="outline">{copy.latest}</Badge> : null}
                      {current ? <Badge variant="outline">{copy.current}</Badge> : null}
                    </div>
                    <div className="space-y-2.5">
                      {release.entries.map((entry, index) => (
                        <div key={`${release.version}-${index}`} className="flex items-start gap-2.5">
                          <Badge variant="outline" className={cn("mt-0.5 shrink-0", categoryStyles[entry.type])}>{copy.categories[entry.type]}</Badge>
                          <p className="min-w-0 text-sm leading-6 text-foreground/80">{chinese ? entry.zh : entry.en}</p>
                        </div>
                      ))}
                    </div>
                  </section>
                );
              })}
            </div>
          </div>

          <div className="shrink-0 border-t p-4">
            <Button variant="outline" className="w-full rounded-md" asChild>
              <a href={`${repositoryURL}/tags`} target="_blank" rel="noreferrer">
                {copy.github}<ExternalLink />
              </a>
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </>
  );
}

function VersionPanel({ label, version, action }: { label: string; version: string; action?: ReactNode }) {
  return (
    <div className="flex min-h-20 flex-col justify-between rounded-md border px-4 py-3">
      <div className="flex items-center justify-between gap-3 text-xs text-muted-foreground"><span>{label}</span>{action}</div>
      <strong className="text-lg font-semibold">{version}</strong>
    </div>
  );
}

const zhCopy = {
  title: "版本更新",
  description: "查看当前版本、最新版本和发布历史。",
  open: "查看版本更新",
  currentVersion: "当前版本",
  latestVersion: "最新版本",
  check: "检查更新",
  checking: "检查中",
  checkFailed: "版本信息暂时不可用",
  latest: "最新",
  current: "当前",
  updateAvailable: "有新版本",
  github: "前往 GitHub 更新",
  categories: { feature: "功能", fix: "修复", security: "安全", ops: "运维", improvement: "优化", style: "界面" },
} as const;

const enCopy = {
  title: "Version updates",
  description: "Review the current version, latest version, and release history.",
  open: "View version updates",
  currentVersion: "Current version",
  latestVersion: "Latest version",
  check: "Check for updates",
  checking: "Checking",
  checkFailed: "Version information is temporarily unavailable",
  latest: "Latest",
  current: "Current",
  updateAvailable: "Update available",
  github: "View updates on GitHub",
  categories: { feature: "Feature", fix: "Fix", security: "Security", ops: "Ops", improvement: "Improvement", style: "Style" },
} as const;
