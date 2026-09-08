// DiffSidebar — the slide-out file-edit + diff rail.
//
// Distinct from ExecutionContextSidebar (which shows run context — untouched).
// This is the left rail that slides out at will on the Ask Orchicon page and
// the execution pages. Three tabs (Diff | Tree | Timeline) at the top. State
// (open/tab/selectedFile) is owned by the HOST (per-page persistent via
// usePersistentState) so it survives navigation; the sidebar renders based on
// the `open` prop.
//
// Data comes exclusively from the diff pipeline RPCs:
//   - GetSessionFileEdits (durable fetch / git-reconciled final ledger)
//   - StreamFileEdits (live events) via the useStream pattern
// merged with the sessionItems.mergeSessionItems discipline (mergeEdits).

import { useMemo } from "react";
import { X } from "lucide-react";

import { cn } from "@/lib/utils";
import { useSessionFileEdits } from "@/api/fileEdits";
import { groupByFile } from "@/lib/diff/sideBySide";
import { DiffView } from "@/components/diffs/DiffView";
import { DiffTimeline } from "@/components/diffs/DiffTimeline";
import { DiffTree } from "@/components/diffs/DiffTree";

export type DiffTab = "diff" | "tree" | "timeline";

export interface DiffSidebarProps {
  open: boolean;
  onClose: () => void;
  ownerKind: "execution" | "ask_conversation";
  ownerId: string;
  isLive?: boolean;
  title?: string;
  tab: DiffTab;
  onTabChange: (tab: DiffTab) => void;
  selectedPath: string;
  onSelectPath: (path: string) => void;
  /** force the unified (single-column) diff fallback on narrow widths */
  unified?: boolean;
}

export function DiffSidebar({
  open,
  onClose,
  ownerKind,
  ownerId,
  isLive = false,
  tab,
  onTabChange,
  selectedPath,
  onSelectPath,
  unified = false,
}: DiffSidebarProps) {
  const { edits, loading } = useSessionFileEdits(ownerKind, ownerId, isLive);

  const files = useMemo(() => groupByFile(edits), [edits]);
  const selected = useMemo(
    () => files.find((f) => f.path === selectedPath) ?? files[0] ?? null,
    [files, selectedPath],
  );
  const effectivePath = selected?.path ?? "";

  if (!open) return null;

  const tabClasses = (t: DiffTab) =>
    cn(
      "px-3 py-1.5 text-xs font-medium rounded-md transition",
      tab === t
        ? "bg-accent text-accent-foreground"
        : "text-muted-foreground hover:text-foreground",
    );

  return (
    <div className="relative flex h-full shrink-0 overflow-hidden border-r border-border/60 bg-background/60 backdrop-blur transition-[width] duration-300 ease-in-out w-[480px] min-w-[480px]">
      <aside className="flex h-full w-[480px] flex-col">
        {/* Tab switcher + close */}
        <div className="flex items-center gap-1 border-b border-border/60 px-3 py-2">
          {(["diff", "tree", "timeline"] as const).map((t) => (
            <button
              key={t}
              type="button"
              onClick={() => onTabChange(t)}
              className={tabClasses(t)}
              aria-pressed={tab === t}
            >
              {t === "diff" ? "Diff" : t === "tree" ? "Tree" : "Timeline"}
            </button>
          ))}
          <button
            type="button"
            onClick={onClose}
            aria-label="Close diff sidebar"
            className="ml-auto flex h-8 w-8 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-foreground"
          >
            <X aria-hidden="true" className="h-4 w-4" />
          </button>
        </div>

        {/* Tab content */}
        {tab === "timeline" && (
          <DiffTimeline files={files} onSelect={(p) => { onSelectPath(p); onTabChange("diff"); }} />
        )}
        {tab === "tree" && (
          <DiffTree files={files} onSelect={(p) => { onSelectPath(p); onTabChange("diff"); }} selectedPath={effectivePath} />
        )}
        {tab === "diff" && (
          <DiffView
            diff={selected?.edits[selected.edits.length - 1]?.unifiedDiff ?? ""}
            path={effectivePath}
            unified={unified}
          />
        )}

        {loading && (
          <div className="border-t border-border/40 px-3 py-1.5 text-[10px] text-muted-foreground">
            Loading diff…
          </div>
        )}
      </aside>
    </div>
  );
}
