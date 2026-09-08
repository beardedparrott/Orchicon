// DiffTimeline — chronological stream of file edits for a session.
//
// Grouped/expandable by file. Each row shows path, kind badge
// (create=emerald, modify=amber, delete=red), triggering tool, and timestamp.
// Clicking a file opens the Diff tab (parent onSelect). Large groups are
// virtualized via @tanstack/react-virtual.

import { useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { FilePlus2, FilePen, FileX2, ChevronRight, ListTree } from "lucide-react";

import { cn } from "@/lib/utils";
import type { FileGroup } from "@/lib/diff/sideBySide";

const VIRTUAL_THRESHOLD = 100;

interface DiffTimelineProps {
  files: FileGroup[];
  onSelect: (path: string) => void;
}

function KindBadge({ kind }: { kind: string }) {
  const styles: Record<string, string> = {
    create: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
    modify: "bg-amber-500/15 text-amber-600 dark:text-amber-400",
    delete: "bg-red-500/15 text-red-600 dark:text-red-400",
  };
  return (
    <span className={cn("rounded px-1 py-0.5 text-[9px] font-semibold uppercase", styles[kind] ?? "bg-muted text-muted-foreground")}>
      {kind || "edit"}
    </span>
  );
}

function kindIcon(kind: string) {
  if (kind === "create") return <FilePlus2 aria-hidden="true" className="h-3.5 w-3.5 text-emerald-500" />;
  if (kind === "delete") return <FileX2 aria-hidden="true" className="h-3.5 w-3.5 text-red-500" />;
  return <FilePen aria-hidden="true" className="h-3.5 w-3.5 text-amber-500" />;
}

function fmtTime(sec: number): string {
  if (!sec) return "";
  const d = new Date(sec * 1000);
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function FileRow({ g, onSelect }: { g: FileGroup; onSelect: (p: string) => void }) {
  const [expanded, setExpanded] = useState(false);
  return (
    <div className="border-b border-border/40">
      <button
        type="button"
        onClick={() => onSelect(g.path)}
        className="flex w-full items-center gap-2 px-3 py-2 text-left hover:bg-accent/50"
      >
        {kindIcon(g.kind)}
        <span className="flex-1 truncate font-mono text-xs">{g.path}</span>
        <KindBadge kind={g.kind} />
        <span className="shrink-0 text-[10px] text-muted-foreground/70">{g.lastTool}</span>
        <span className="shrink-0 text-[10px] text-muted-foreground/60">{fmtTime(g.lastAt)}</span>
        <button
          type="button"
          onClick={(e) => {
            e.stopPropagation();
            setExpanded((v) => !v);
          }}
          aria-label={expanded ? "Collapse edits" : "Expand edits"}
          className="shrink-0 text-muted-foreground hover:text-foreground"
        >
          <ChevronRight aria-hidden="true" className={cn("h-3.5 w-3.5 transition-transform", expanded ? "rotate-90" : "")} />
        </button>
      </button>
      {expanded && (
        <div className="border-l border-border/50 py-1 pl-8 pr-3">
          {g.edits.map((e) => (
            <div key={e.id} className="flex items-center gap-2 py-0.5 text-[10px] text-muted-foreground">
              <span className="shrink-0 rounded bg-muted px-1 font-mono">{e.kind}</span>
              <span className="shrink-0 font-mono">{e.tool}</span>
              <span className="ml-auto shrink-0">{fmtTime(e.createdAt?.seconds ? Number(e.createdAt.seconds) : 0)}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export function DiffTimeline({ files, onSelect }: DiffTimelineProps) {
  const parentRef = useRef<HTMLDivElement>(null);
  const virtual = files.length > VIRTUAL_THRESHOLD;
  const virtualizer = useVirtualizer({
    count: files.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => 40,
    overscan: 10,
  });

  if (files.length === 0) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center p-8 text-center text-sm text-muted-foreground">
        <ListTree aria-hidden="true" className="mb-3 h-8 w-8 opacity-40" />
        No file edits for this session yet.
      </div>
    );
  }

  return (
    <div ref={parentRef} className={cn("flex-1 overflow-y-auto", virtual ? "relative" : "")}>
      {virtual ? (
        <div className="relative" style={{ height: `${virtualizer.getTotalSize()}px` }}>
          {virtualizer.getVirtualItems().map((vi) => {
            const g = files[vi.index];
            return (
              <div
                key={g.path}
                className="absolute left-0 top-0 w-full"
                style={{ height: `${vi.size}px`, transform: `translateY(${vi.start}px)` }}
              >
                <FileRow g={g} onSelect={onSelect} />
              </div>
            );
          })}
        </div>
      ) : (
        files.map((g) => <FileRow key={g.path} g={g} onSelect={onSelect} />)
      )}
    </div>
  );
}
