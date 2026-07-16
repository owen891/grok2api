export function SiteFooter() {
  return (
    <footer className="fixed right-0 bottom-0 z-20 flex h-10 w-fit max-w-[calc(100vw-2rem)] items-center justify-end gap-1.5 whitespace-nowrap px-5 text-right text-[11px] text-muted-foreground sm:px-6">
      <a className="transition-colors hover:text-foreground" href="https://github.com/owen891/grok2api" target="_blank" rel="noreferrer">owen891/grok2api</a>
      <span>© 2026</span>
      <span aria-hidden="true">·</span>
      <span>Forked from</span>
      <a className="transition-colors hover:text-foreground" href="https://github.com/chenyme/grok2api" target="_blank" rel="noreferrer">chenyme/grok2api</a>
    </footer>
  );
}
