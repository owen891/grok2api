from pathlib import Path
import re

# Fix duplicated class on app-shell content wrapper
shell_path = Path(r"E:/www/grok2api-v3-migration/frontend/src/app/app-shell.tsx")
shell = shell_path.read_text(encoding="utf-8")
shell = shell.replace(
    'className="flex min-h-screen flex-col flex min-h-screen flex-col lg:pl-[288px]"',
    'className="flex min-h-screen flex-col lg:pl-[288px]"',
)
shell = shell.replace(
    'className="flex min-h-screen flex-col min-h-screen flex flex-col lg:pl-[288px]"',
    'className="flex min-h-screen flex-col lg:pl-[288px]"',
)
# also mobile drawer wrappers if needed
shell_path.write_text(shell, encoding="utf-8")
print("shell fixed")

# Rewrite chat page layout for adaptive fill
cp = Path(r"E:/www/grok2api-v3-migration/frontend/src/features/chat/chat-page.tsx")
ct = cp.read_text(encoding="utf-8")

if "Menu" not in ct.split('from "lucide-react"')[0]:
    ct = ct.replace(
        'import { Loader2, MessageSquarePlus, Trash2 } from "lucide-react";',
        'import { Loader2, Menu, MessageSquarePlus, Trash2, X } from "lucide-react";',
        1,
    )

if "const [sidebarOpen, setSidebarOpen]" not in ct:
    ct = ct.replace(
        'const [draft, setDraft] = useState("");\n  const [sending, setSending] = useState(false);',
        'const [draft, setDraft] = useState("");\n  const [sending, setSending] = useState(false);\n  const [sidebarOpen, setSidebarOpen] = useState(false);',
        1,
    )

# Find the main return of ChatPage (not the early empty return)
# Prefer the large layout return with h-[calc...]
start = ct.find('  return (\n    <div className="flex h-[calc(100vh-7rem)]')
if start < 0:
    # fallback: second return
    first = ct.find("  return (")
    start = ct.find("  return (", first + 1)
print("layout start", start)
if start < 0:
    raise SystemExit("layout return not found")

# End at component closing brace
end = ct.rfind("\n}\n")
if end < start:
    end = len(ct)

new_layout = r'''  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="relative flex min-h-[min(78vh,820px)] flex-1 overflow-hidden rounded-2xl border border-border/60 bg-gradient-to-br from-background via-background to-muted/20 shadow-sm lg:min-h-0">
        {sidebarOpen ? (
          <button
            type="button"
            className="absolute inset-0 z-20 bg-black/40 lg:hidden"
            aria-label="关闭会话列表"
            onClick={() => setSidebarOpen(false)}
          />
        ) : null}

        <aside
          className={`absolute inset-y-0 left-0 z-30 flex w-[min(86vw,18rem)] flex-col border-r border-border/50 bg-card/95 backdrop-blur transition-transform duration-200 lg:static lg:z-0 lg:w-72 ${
            sidebarOpen ? "translate-x-0" : "-translate-x-full lg:translate-x-0"
          }`}
        >
          <div className="flex items-center justify-between px-4 py-3">
            <div>
              <div className="text-sm font-semibold tracking-tight">{t("chat.sessions")}</div>
              <div className="text-[11px] text-muted-foreground">本地保存，不存密钥</div>
            </div>
            <div className="flex items-center gap-1">
              <Button type="button" size="sm" variant="secondary" className="rounded-full" onClick={onCreateSession} disabled={sending}>
                <MessageSquarePlus className="h-4 w-4" />
              </Button>
              <Button type="button" size="sm" variant="ghost" className="rounded-full lg:hidden" onClick={() => setSidebarOpen(false)}>
                <X className="h-4 w-4" />
              </Button>
            </div>
          </div>
          <div className="min-h-0 flex-1 space-y-1 overflow-y-auto px-2 pb-3">
            {prefs.sessions.map((session) => {
              const active = session.id === activeSession.id;
              return (
                <div
                  key={session.id}
                  className={`group flex items-center gap-1 rounded-xl px-2.5 py-2.5 text-sm transition ${
                    active ? "bg-primary/12 text-primary shadow-sm" : "hover:bg-muted/70"
                  }`}
                >
                  <button
                    type="button"
                    className="min-w-0 flex-1 truncate text-left"
                    onClick={() => {
                      setPrefs((prev) => ({ ...prev, activeSessionId: session.id }));
                      setSidebarOpen(false);
                    }}
                    disabled={sending}
                  >
                    {session.title || t("chat.untitled")}
                    <div className="text-[11px] opacity-60">{session.mode === "image" ? "生图" : "对话"}</div>
                  </button>
                  <button
                    type="button"
                    className="rounded p-1 opacity-0 transition group-hover:opacity-100 hover:bg-background"
                    onClick={() => onDeleteSession(session.id)}
                    disabled={sending}
                    aria-label={t("chat.deleteSession")}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </button>
                </div>
              );
            })}
          </div>
          <div className="border-t border-border/50 p-3">
            <label className="mb-1 block text-[11px] font-medium text-muted-foreground">{t("chat.clientKey")}</label>
            <select
              value={prefs.clientKeyId ?? ""}
              disabled={loadingMeta || sending}
              onChange={(event) =>
                setPrefs((prev) => ({
                  ...prev,
                  clientKeyId: event.target.value || null,
                }))
              }
              className="h-9 w-full rounded-lg border border-border/60 bg-background/80 px-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              <option value="">{t("chat.clientKeyPlaceholder")}</option>
              {clientKeys.map((key) => (
                <option key={key.id} value={key.id}>
                  {key.name} ({key.prefix}…)
                </option>
              ))}
            </select>
            {clientKeys.length === 0 && !loadingMeta ? (
              <p className="mt-1 text-xs text-muted-foreground">
                {t("chat.noClientKeys")}{" "}
                <Link to="/client-keys" className="text-primary underline-offset-2 hover:underline">
                  {t("chat.manageKeys")}
                </Link>
              </p>
            ) : null}
            {metaError ? <p className="mt-2 text-xs text-destructive">{metaError}</p> : null}
            {loadingMeta || loadingSecret ? (
              <div className="mt-2 flex items-center gap-2 text-xs text-muted-foreground">
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
                {loadingSecret ? t("chat.loadingSecret") : t("chat.loadingMeta")}
              </div>
            ) : null}
          </div>
        </aside>

        <section className="flex min-h-0 min-w-0 flex-1 flex-col">
          <div className="flex items-center justify-between gap-3 border-b border-border/50 px-3 py-2.5 sm:px-4 sm:py-3">
            <div className="flex min-w-0 items-center gap-2">
              <Button type="button" size="sm" variant="ghost" className="rounded-full lg:hidden" onClick={() => setSidebarOpen(true)}>
                <Menu className="h-4 w-4" />
              </Button>
              <div className="min-w-0">
                <h1 className="truncate text-sm font-semibold tracking-tight sm:text-base md:text-lg">
                  {activeSession.title || t("chat.untitled")}
                </h1>
                <p className="truncate text-[11px] text-muted-foreground sm:text-xs">
                  {activeSession.mode === "image" ? "对话内生图 · 结果直接显示在会话中" : t("chat.subtitle")}
                </p>
              </div>
            </div>
            <div className="hidden items-center gap-2 sm:flex">
              <span
                className={`rounded-full px-2.5 py-1 text-[11px] ${
                  activeSession.mode === "image"
                    ? "bg-cyan-500/15 text-cyan-700 dark:text-cyan-300"
                    : "bg-muted text-muted-foreground"
                }`}
              >
                {activeSession.mode === "image" ? "生图模式" : "对话模式"}
              </span>
              <span className="max-w-[10rem] truncate text-xs text-muted-foreground md:max-w-[16rem]">
                {activeSession.mode === "image"
                  ? activeModel || imageModels[0] || "grok-imagine-image"
                  : activeModel || chatModels[0] || "-"}
              </span>
            </div>
          </div>

          <div ref={threadRef} className="min-h-0 flex-1 overflow-y-auto px-3 py-3 sm:px-4 sm:py-4">
            <MessageList messages={activeSession.messages} emptyHint={t("chat.threadEmpty")} />
          </div>

          <div className="border-t border-border/50 px-3 py-2.5 sm:px-4 sm:py-3">
            <Composer
              mode={activeSession.mode}
              value={draft}
              sending={sending}
              disabled={!prefs.clientKeyId || !clientSecret || loadingSecret}
              chatModels={chatModels}
              imageModels={imageModels}
              model={activeModel}
              imageSettings={activeSession.imageSettings}
              onChange={setDraft}
              onSend={() => void send()}
              onStop={stop}
              onModeChange={(mode: ChatMode) =>
                updateSession(activeSession.id, (session) => ({
                  ...session,
                  mode,
                  model:
                    mode === "image"
                      ? pickImageModel(imageModels, session.imageSettings.quality)
                      : session.model && chatModels.includes(session.model)
                        ? session.model
                        : chatModels[0] || "",
                  updatedAt: Date.now(),
                }))
              }
              onModelChange={(model) =>
                updateSession(activeSession.id, (session) => ({
                  ...session,
                  model,
                  updatedAt: Date.now(),
                }))
              }
              onImageSettingsChange={(imageSettings: ImageSettings) =>
                updateSession(activeSession.id, (session) => ({
                  ...session,
                  imageSettings,
                  model: pickImageModel(imageModels, imageSettings.quality),
                  updatedAt: Date.now(),
                }))
              }
            />
          </div>
        </section>
      </div>
    </div>
  );
}
'''

ct = ct[:start] + new_layout
cp.write_text(ct, encoding="utf-8")
print("chat-page rewritten")
print("brace diff", ct.count("{") - ct.count("}"))
print("sidebarOpen", "sidebarOpen" in ct)
print("Menu", "Menu" in ct)
print("X", ", X" in ct or " X " in ct)
