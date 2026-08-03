import { createRoute, Link } from "@tanstack/react-router";
import { useState } from "react";
import { Trash2, SearchX, Boxes, Loader2, CheckCircle2, XCircle } from "lucide-react";

import {
  useListRuntimeImages,
  useDeleteRuntimeImage,
  useAvailableRuntimeImages,
} from "@/api/runtimeImages";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";
import type { RuntimeImage } from "@/api/gen/orchicon/api/v1/runtime_image_pb";

// Runtime images page: tenant container image specs built by the runtime
// daemon. Workers execute in a per-workflow-run runtime container whose
// image is chosen per work item; this page is where those images are
// defined and built.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/runtime-images",
  component: RuntimeImagesPage,
});

const STATUS_LABELS: Record<number, string> = {
  1: "draft",
  2: "building",
  3: "ready",
  4: "failed",
};

const STATUS_STYLES: Record<number, string> = {
  1: "bg-gray-100 text-gray-700",
  2: "bg-blue-100 text-blue-800",
  3: "bg-green-600 text-white",
  4: "bg-red-100 text-red-800",
};

function StatusBadge({ status }: { status: number }) {
  const Icon =
    status === 2 ? (
      <Loader2 className="h-3 w-3 animate-spin" />
    ) : status === 3 ? (
      <CheckCircle2 className="h-3 w-3" />
    ) : status === 4 ? (
      <XCircle className="h-3 w-3" />
    ) : (
      <Boxes className="h-3 w-3" />
    );
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium",
        STATUS_STYLES[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {Icon}
      {STATUS_LABELS[status] ?? "unknown"}
    </span>
  );
}

function RuntimeImagesPage() {
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const batchDelete = useDeleteRuntimeImage();

  const { data: images, isLoading, error } = useListRuntimeImages(
    statusFilter ? Number(statusFilter) : undefined,
    search || undefined,
  );
  const { data: available } = useAvailableRuntimeImages();
  // Stock images = the daemon's shipped images (base first). Custom
  // images with no spec row are derived by removing the stock ones.
  const stockImages = available?.stockImages ?? [];

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const toggleSelectAll = (items: RuntimeImage[]) => {
    if (selected.size === items.length) {
      setSelected(new Set());
    } else {
      setSelected(new Set(items.map((i) => i.id)));
    }
  };

  const handleBatchDelete = () => {
    if (selected.size === 0) return;
    const count = selected.size;
    if (
      !window.confirm(
        `Delete ${count} runtime image${count === 1 ? "" : "s"}? This also removes the local Docker image${
          count === 1 ? "" : "s"
        }.`,
      )
    ) {
      return;
    }
    for (const id of selected) {
      batchDelete.mutate(id);
    }
    setSelected(new Set());
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Runtime Images</h1>
          <p className="text-sm text-muted-foreground">
            Container images workers run in, built by Orchicon from a
            base image you always inherit. Pick one per work item.
          </p>
        </div>
        <Button asChild>
          <Link to="/runtime-images/new">New Runtime Image</Link>
        </Button>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <Input
          placeholder="Search name or slug…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="h-9 w-56"
        />
        <select
          className="rounded-md border bg-background px-3 py-1.5 text-sm"
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value)}
        >
          <option value="">All statuses</option>
          <option value="1">draft</option>
          <option value="2">building</option>
          <option value="3">ready</option>
          <option value="4">failed</option>
        </select>

        {selected.size > 0 && (
          <Button
            variant="destructive"
            size="sm"
            onClick={handleBatchDelete}
            disabled={batchDelete.isPending}
          >
            <Trash2 className="mr-1 h-3.5 w-3.5" />
            Delete {selected.size} selected
          </Button>
        )}
      </div>

      {/* Stock images shipped with Orchicon (read-only; base first). */}
      {stockImages.length > 0 && (
        <div>
          <h2 className="mb-2 text-sm font-semibold text-muted-foreground">
            Stock images (shipped with Orchicon)
          </h2>
          <div className="space-y-2">
            {stockImages.map((tag) => (
              <Card key={tag} className="opacity-80">
                <CardContent className="flex items-center gap-3 p-3">
                  <Boxes className="h-4 w-4 shrink-0 text-muted-foreground" />
                  <span className="min-w-0 flex-1 truncate font-mono text-sm">
                    {tag}
                  </span>
                  {tag === (available?.defaultImage ?? "") && (
                    <span className="rounded-full bg-green-100 px-2 py-0.5 text-xs font-medium text-green-800">
                      default
                    </span>
                  )}
                </CardContent>
              </Card>
            ))}
          </div>
        </div>
      )}

      {isLoading && (
        <p className="text-sm text-muted-foreground">Loading…</p>
      )}
      {error && (
        <p className="text-sm text-destructive">
          Failed to load runtime images: {String(error)}
        </p>
      )}
      {!isLoading && !error && (!images || images.length === 0) && (
        <div>
          <h2 className="mb-2 text-sm font-semibold text-muted-foreground">
            Custom images (built by Orchicon)
          </h2>
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <SearchX className="h-5 w-5 text-muted-foreground" />
                No custom runtime images yet
              </CardTitle>
              <CardDescription>
                Create an image to define the toolchain/system libraries your
                workers get in their runtime container.
              </CardDescription>
            </CardHeader>
          </Card>
        </div>
      )}

      {images && images.length > 0 && (
        <div className="space-y-2">
          <h2 className="text-sm font-semibold text-muted-foreground">
            Custom images (built by Orchicon)
          </h2>
          <div className="flex items-center gap-2 px-2 py-1">
            <input
              type="checkbox"
              checked={selected.size === images.length && images.length > 0}
              onChange={() => toggleSelectAll(images)}
              className="h-4 w-4 rounded border-input"
            />
            <span className="text-xs text-muted-foreground">
              {selected.size > 0
                ? `${selected.size} of ${images.length} selected`
                : `${images.length} runtime image${images.length === 1 ? "" : "s"}`}
            </span>
          </div>
          {images.map((img) => (
            <Card key={img.id} className="transition-colors hover:bg-accent/50">
              <CardContent className="flex items-center gap-3 p-3">
                <input
                  type="checkbox"
                  checked={selected.has(img.id)}
                  onChange={() => toggleSelect(img.id)}
                  className="h-4 w-4 shrink-0 rounded border-input"
                />
                <StatusBadge status={img.status} />
                <Link
                  to="/runtime-images/$id"
                  params={{ id: img.id }}
                  className="min-w-0 flex-1"
                >
                  <p className="truncate text-sm font-medium hover:underline">
                    {img.name}
                  </p>
                  <p className="truncate text-xs text-muted-foreground">
                    {img.slug || img.tag || "no tag"}
                  </p>
                </Link>
                {img.error && (
                  <span className="max-w-64 truncate text-xs text-destructive">
                    {img.error}
                  </span>
                )}
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}
