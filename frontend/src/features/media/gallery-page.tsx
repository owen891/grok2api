import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Database, Image as ImageIcon, RefreshCw, Search, type LucideIcon } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { ImageLightbox, type LightboxImage } from "@/features/chat/image-lightbox";
import { clearImages, deleteImage, getImageStats, listImages } from "@/features/media/media-api";
import type { MediaAssetDTO } from "@/features/media/types";
import { EmptyState, ErrorState, LoadingState } from "@/shared/components/data-state";
import { PageHeader } from "@/shared/components/page-header";
import { Pagination } from "@/shared/components/pagination";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

export function GalleryPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [preview, setPreview] = useState<LightboxImage | null>(null);
  const [clearOpen, setClearOpen] = useState(false);
  const debouncedSearch = useDebouncedValue(search);
  const normalizedSearch = debouncedSearch.trim();

  const imagesQuery = useQuery({
    queryKey: ["media", "images", page, pageSize, normalizedSearch],
    queryFn: () => listImages({ page, pageSize, search: normalizedSearch || undefined }),
  });
  const statsQuery = useQuery({
    queryKey: ["media", "images", "stats"],
    queryFn: getImageStats,
    staleTime: 30_000,
  });
  const deleteMutation = useMutation({ mutationFn: deleteImage, onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["media", "images"] }); } });
  const clearMutation = useMutation({ mutationFn: clearImages, onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["media", "images"] }); } });

  const result = imagesQuery.data;
  const refreshing = imagesQuery.isFetching || statsQuery.isFetching;

  function refreshAll(): void {
    void imagesQuery.refetch();
    void statsQuery.refetch();
  }

  return (
    <div className="space-y-8">
      <PageHeader
        title={t("media.images.title")}
        description={t("media.images.description")}
        actions={(
          <div className="flex gap-2">
            <Button variant="secondary" size="sm" onClick={refreshAll} disabled={refreshing}><RefreshCw className={refreshing ? "animate-spin" : undefined} />{t("common.refresh")}</Button>
            <Button variant="destructive" size="sm" onClick={() => setClearOpen(true)} disabled={clearMutation.isPending || (statsQuery.data?.totalImages ?? 0) === 0}>{t("media.images.clear")}</Button>
          </div>
        )}
      />

      <section className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
        <MediaMetric icon={ImageIcon} loading={statsQuery.isPending} label={t("media.images.totalImages")} value={formatNumber(statsQuery.data?.totalImages ?? 0, i18n.language, 0)} detail={t("media.images.totalImagesDetail")} />
        <MediaMetric icon={Database} loading={statsQuery.isPending} label={t("media.images.totalBytes")} value={formatBytes(statsQuery.data?.totalBytes ?? 0, i18n.language)} detail={t("media.images.totalBytesDetail")} />
      </section>

      <section className="space-y-4">
        <div className="flex min-h-12 flex-wrap items-center justify-between gap-3 py-2">
          <div className="relative w-full sm:w-80">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              className="h-8 pl-9 text-xs"
              value={search}
              onChange={(event) => { setSearch(event.target.value); setPage(1); }}
              placeholder={t("media.images.search")}
              aria-label={t("media.images.search")}
            />
          </div>
          {result ? <span className="text-xs text-muted-foreground">{t("media.images.pageSummary", { count: result.items.length, total: result.total })}</span> : null}
        </div>

        {imagesQuery.isError ? <ErrorState message={imagesQuery.error.message} onRetry={() => void imagesQuery.refetch()} /> : null}
        {imagesQuery.isPending ? <LoadingState /> : null}
        {!imagesQuery.isPending && result && result.items.length === 0 ? <EmptyState message={t(normalizedSearch ? "media.images.noMatches" : "media.images.empty")} /> : null}

        {!imagesQuery.isPending && result && result.items.length > 0 ? (
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
            {result.items.map((image) => (
              <ImageCard
                key={image.id}
                image={image}
                locale={i18n.language}
                onOpen={() => setPreview({ url: image.url, name: image.id })}
                onDelete={() => deleteMutation.mutate(image.id)}
              />
            ))}
          </div>
        ) : null}

        {result && result.total > 0 ? (
          <Pagination
            page={result.page}
            pageSize={result.pageSize}
            total={result.total}
            onPageChange={setPage}
            onPageSizeChange={(value) => { setPageSize(value); setPage(1); }}
          />
        ) : null}
      </section>

      <ImageLightbox key={preview ? `${preview.url}:${preview.index ?? 0}` : "closed"} image={preview} onClose={() => setPreview(null)} />
      <AlertDialog open={clearOpen} onOpenChange={setClearOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("media.images.clearTitle")}</AlertDialogTitle>
            <AlertDialogDescription>{t("media.images.clearDescription", { count: statsQuery.data?.totalImages ?? 0 })}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={clearMutation.isPending} onClick={(event) => { event.preventDefault(); clearMutation.mutate(undefined, { onSuccess: () => setClearOpen(false) }); }}>{t("media.images.clear")}</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function ImageCard({
  image,
  locale,
  onOpen,
  onDelete,
}: {
  image: MediaAssetDTO;
  locale: string;
  onOpen: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  return (
    <div
      role="button"
      tabIndex={0}
      onClick={onOpen}
      onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); onOpen(); } }}
      className="group w-full overflow-hidden rounded-lg bg-card text-left transition-colors hover:bg-secondary/45 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
    >
      <div className="aspect-square overflow-hidden bg-muted">
        <img src={image.url} alt={image.id} loading="lazy" className="size-full object-cover transition-transform duration-200 group-hover:scale-[1.02]" />
      </div>
      <div className="space-y-2 p-3 text-xs">
        <div className="flex min-w-0 items-center justify-between gap-2">
          <span className="min-w-0 flex-1 truncate font-medium" title={image.id}>{image.id}</span>
          <span className="shrink-0 text-muted-foreground">{formatBytes(image.sizeBytes, locale)}</span>
        </div>
        <div className="flex min-w-0 items-center justify-between gap-2 text-[11px] text-muted-foreground">
          <span className="min-w-0 truncate" title={image.mimeType}>{image.mimeType || image.kind}</span>
          <span className="shrink-0 whitespace-nowrap">{formatDateTime(image.createdAt, locale)}</span>
        </div>
        <div className="truncate font-mono text-[10px] text-muted-foreground/75" title={image.sha256}>{t("media.images.sha256")}: {image.sha256}</div>
        <Button type="button" variant="ghost" size="sm" className="h-7 w-full text-destructive" onClick={(event) => { event.stopPropagation(); onDelete(); }}>{t("common.delete")}</Button>
      </div>
    </div>
  );
}

function MediaMetric({ icon: Icon, label, value, detail, loading }: { icon: LucideIcon; label: string; value: string; detail: string; loading: boolean }) {
  return (
    <div className="min-h-28 rounded-lg bg-card p-4">
      <div className="flex items-center justify-between gap-3">
        <span className="text-xs text-muted-foreground">{label}</span>
        <Icon className="size-4 shrink-0 text-muted-foreground" />
      </div>
      <div className="mt-3 flex min-h-7 items-center text-xl font-medium tabular-nums">{loading ? <Spinner /> : value}</div>
      <p className="mt-1 text-xs text-muted-foreground">{detail}</p>
    </div>
  );
}

function formatBytes(value: number, locale: string): string {
  if (!Number.isFinite(value) || value <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let size = value;
  let unitIndex = 0;
  while (size >= 1024 && unitIndex < units.length - 1) {
    size /= 1024;
    unitIndex += 1;
  }
  return `${new Intl.NumberFormat(locale, { maximumFractionDigits: unitIndex === 0 || size >= 10 ? 0 : 1 }).format(size)} ${units[unitIndex]}`;
}
