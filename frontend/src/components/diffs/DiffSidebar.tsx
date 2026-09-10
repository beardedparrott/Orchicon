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

import { useEffect, useMemo, useState } from "react";
import { X } from "lucide-react";

import { cn } from "@/lib/utils";
import { useSessionFileEdits } from "@/api/fileEdits";
import { groupByFile } from "@/lib/diff/sideBySide";
import { DiffView } from "@/components/diffs/DiffView";
import { DiffTimeline } from "@/components/diffs/DiffTimeline";
import { DiffTree } from "@/components/diffs/DiffTree";

// Below this viewport width the diff rail is un-affordable as an inline
// sibling of the chat (a 480px rail would push the chat column out of view —
// the acceptance criteria require the chat to stay fully visible and
// interactive alongside the sidebar). On narrow screens we render the same
// sidebar contents as an overlay drawer that slides over the chat, so the
// chat remains visible underneath and interactive after close. Mirrors the
// existing mobile-drawer idiom in __root.tsx (fixed inset-0 + scrim).
const MOBILE_BREAKPOINT = 768;

function useIsNarrow(): boolean {
  const [narrow, setNarrow] = useState(
    () => typeof window !== "undefined" && window.innerWidth < MOBILE_BREAKPOINT,
  );
  useEffect(() => {
    const mq = window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT - 1}px)`);
    const update = () => setNarrow(mq.matches);
    update();
    mq.addEventListener?.("change", update);
    return () => mq.removeEventListener?.("change", update);
  }, []);
  return narrow;
}

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
  const { edits, loading, error } = useSessionFileEdits(ownerKind, ownerId, isLive);
  const narrow = useIsNarrow();

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

  // On narrow viewports the 480px rail can't sit inline with the chat —
  // render the same contents as an overlay drawer so the chat stays visible
  // and interactive. On desktop keep the inline rail (first flex child) with
  // the chat flex-1/min-w-0 alongside.
  if (narrow) {
    return (
      <div className="fixed inset-0 z-50" role="dialog" aria-modal="true" aria-label="Diff sidebar">
        <div className="absolute inset-0 bg-black/50" onClick={onClose} aria-hidden="true" />
        <div className="absolute inset-y-0 left-0 flex w-[min(480px,88vw)] max-w-full shrink-0 flex-col overflow-hidden border-r border-border/60 bg-background shadow-xl">
          <TabHeader tab={tab} onTabChange={onTabChange} onClose={onClose} tabClasses={tabClasses} />
          {error && <LedgerErrorBanner error={error} />}
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
              unified
            />
          )}
          {loading && (
            <div className="border-t border-border/40 px-3 py-1.5 text-[10px] text-muted-foreground">
              Loading diff…
            </div>
          )}
        </div>
      </div>
    );
  }

  return (
    <div className="relative flex h-full shrink-0 overflow-hidden border-r border-border/60 bg-background/60 backdrop-blur transition-[width] duration-300 ease-in-out w-[480px] min-w-[480px]">
      <aside className="flex h-full w-[480px] flex-col">
        <TabHeader tab={tab} onTabChange={onTabChange} onClose={onClose} tabClasses={tabClasses} />
        {error && <LedgerErrorBanner error={error} />}
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

// LedgerErrorBanner — explicit failure state for the diff pipeline. Rendered
// ABOVE the tab content whenever the durable fetch or the live stream fails,
// so an unreachable/broken ledger never masquerades as the "No file edits"
// empty state (DiffTimeline/DiffTree empty text is then reachable only when
// the fetch succeeded AND the ledger is genuinely empty).
function LedgerErrorBanner({ error }: { error: Error }) {
  return (
    <div
      role="alert"
      className="border-b border-destructive/40 bg-destructive/10 px-3 py-2 text-xs text-destructive"
    >
      Couldn&apos;t load file edits: {error.message || "connection failed"}. The
      ledger may have entries that aren&apos;t shown.
    </div>
  );
}

function TabHeader({ tab, onTabChange, onClose, tabClasses }: {
  tab: DiffTab;
  onTabChange: (t: DiffTab) => void;
  onClose: () => void;
  tabClasses: (t: DiffTab) => string;
}) {
  return (
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
  );
}
