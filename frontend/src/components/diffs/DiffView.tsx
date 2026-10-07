// DiffView — side-by-side unified-diff renderer.
//
// Renders the server-computed `FileEdit.unified_diff` for one path as paired
// old|new columns with line numbers (opencode-style), line-level red/green
// highlighting via Tailwind HSL theme tokens, and word-level intra-line
// emphasis where cheap (emphasizeTokens). Syntax-highlighted where the
// language is detectable — but no highlighter is installed in deps (verified:
// none of shiki/highlight.js/prism), so we render plain monospace with the
// line/word coloring to avoid a new dependency.
//
// Large diffs are virtualized via @tanstack/react-virtual so a 5k-line diff
// scrolls without jank. Rows WRAP (`whitespace-pre-wrap` + `overflow-wrap:
// anywhere`), so their height is variable, not a fixed `leading-5` line: the
// virtualizer MEASURES each row (`measureElement` on one absolutely-positioned
// grid wrapper) instead of assuming an estimate. Both columns of a row live in
// one CSS grid, so they share the row's measured height by construction and
// cannot drift apart while scrolling.

import { useMemo, useRef } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { FileCode2, Plus, Minus } from "lucide-react";

import { cn } from "@/lib/utils";
import { parseUnifiedDiff, type SideBySideRow } from "@/lib/diff/sideBySide";

interface DiffViewProps {
  diff: string;
  path: string;
  /** force the unified (single-column) fallback — used on narrow widths */
  unified?: boolean;
}

function rowKey(r: SideBySideRow, i: number): string {
  return `${r.kind}-${r.lineNoOld ?? "x"}-${r.lineNoNew ?? "x"}-${i}`;
}

/** Renders an emphasis-span-wrapped line for word-level intra-line marking. */
function renderLine(
  text: string,
  spans: SideBySideRow["oldSpans"],
  tone: "add" | "del",
): React.ReactNode {
  if (!spans || spans.length === 0) return text;
  const out: React.ReactNode[] = [];
  let cursor = 0;
  const sorted = [...spans].sort((a, b) => a.start - b.start);
  for (const s of sorted) {
    if (s.start > cursor) out.push(text.slice(cursor, s.start));
    out.push(
      <span
        key={s.start}
        className={cn(
          tone === "add"
            ? "bg-emerald-500/25 dark:bg-emerald-500/30"
            : "bg-red-500/25 dark:bg-red-500/30",
          "rounded-[2px]",
        )}
      >
        {text.slice(s.start, s.end)}
      </span>,
    );
    cursor = s.end;
  }
  if (cursor < text.length) out.push(text.slice(cursor));
  return out;
}

function rowClasses(r: SideBySideRow): string {
  if (r.kind === "add") return "bg-emerald-500/10 text-emerald-700 dark:text-emerald-400";
  if (r.kind === "del") return "bg-red-500/10 text-red-700 dark:text-red-300";
  return "text-foreground/80";
}

function LineNo({ value, tone }: { value: number | null; tone: "old" | "new" }) {
  return (
    <span
      className={cn(
        "select-none shrink-0 w-10 pr-2 text-right font-mono text-[10px] leading-5",
        tone === "old" ? "text-muted-foreground/60" : "text-muted-foreground/60",
      )}
    >
      {value ?? ""}
    </span>
  );
}

function SideBySideBody({ rows }: { rows: SideBySideRow[] }) {
  const parentRef = useRef<HTMLDivElement>(null);
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => 20,
    overscan: 12,
    // Key the measurement cache by row identity, not index, so heights are not
    // poisoned across file switches (the row set changes under the virtualizer).
    getItemKey: (i) => rowKey(rows[i], i),
  });

  return (
    <div ref={parentRef} className="diff-scroll h-full overflow-auto">
      <div className="relative w-full" style={{ height: `${virtualizer.getTotalSize()}px` }}>
        {virtualizer.getVirtualItems().map((vi) => {
          const r = rows[vi.index];
          const bg = rowClasses(r);
          return (
            // ONE measured, absolutely-positioned row wrapper (no inline height
            // — a fixed height would make measurement return the estimate
            // forever). A 2-column grid stretches BOTH halves to the measured
            // content height, so the columns cannot drift apart.
            <div
              key={rowKey(r, vi.index)}
              data-index={vi.index}
              ref={virtualizer.measureElement}
              className="absolute left-0 top-0 grid w-full grid-cols-2"
              style={{ transform: `translateY(${vi.start}px)` }}
            >
              <div className={cn("flex min-w-0 items-stretch whitespace-pre-wrap", bg)}>
                <LineNo value={r.lineNoOld} tone="old" />
                <span className="w-4 shrink-0" />
                <span className="min-w-0 flex-1 font-mono text-xs leading-5 [overflow-wrap:anywhere]">
                  {renderLine(r.oldText, r.oldSpans, "del")}
                </span>
              </div>
              <div className={cn("flex min-w-0 items-stretch whitespace-pre-wrap", bg)}>
                <LineNo value={r.lineNoNew} tone="new" />
                <span className="w-4 shrink-0" />
                <span className="min-w-0 flex-1 font-mono text-xs leading-5 [overflow-wrap:anywhere]">
                  {renderLine(r.newText, r.newSpans, "add")}
                </span>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function UnifiedBody({ rows }: { rows: SideBySideRow[] }) {
  return (
    <div className="diff-scroll h-full overflow-auto">
      <div className="min-w-0 py-1">
        {rows.map((r, i) => (
          <div
            key={rowKey(r, i)}
            className={cn("flex min-w-0 items-stretch whitespace-pre-wrap", rowClasses(r))}
          >
            <span className="w-6 shrink-0 text-center font-mono text-[10px] text-muted-foreground/50">
              {r.sign}
            </span>
            <span className="shrink-0 px-1 font-mono text-[10px] text-muted-foreground/50">
              {r.lineNoOld ?? ""}
            </span>
            <span className="shrink-0 px-1 font-mono text-[10px] text-muted-foreground/50">
              {r.lineNoNew ?? ""}
            </span>
            <span className="min-w-0 flex-1 px-2 font-mono text-xs leading-5 [overflow-wrap:anywhere]">
              {r.kind === "add"
                ? renderLine(r.newText, r.newSpans, "add")
                : r.kind === "del"
                  ? renderLine(r.oldText, r.oldSpans, "del")
                  : r.oldText}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}

export function DiffView({ diff, path, unified = false }: DiffViewProps) {
  const rows = useMemo(() => parseUnifiedDiff(diff), [diff]);
  const adds = rows.filter((r) => r.kind === "add").length;
  const dels = rows.filter((r) => r.kind === "del").length;
  // Side-by-side is the primary view; the unified (single-column) mode is the
  // narrow-width fallback driven by the host via the `unified` prop. The
  // virtualizer inside SideBySideBody windows large diffs regardless.
  const useSideBySide = !unified;

  if (rows.length === 0) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center p-8 text-center text-sm text-muted-foreground">
        <FileCode2 aria-hidden="true" className="mb-3 h-10 w-10 opacity-40" />
        {diff ? (
          <span>No renderable diff (binary / truncated).</span>
        ) : (
          <span>Select a file to view its diff.</span>
        )}
      </div>
    );
  }

  return (
    <div className="flex flex-1 flex-col min-h-0">
      <div className="flex shrink-0 items-center gap-2 border-b border-border/60 px-3 py-2">
        <span className="truncate font-mono text-xs">{path}</span>
        <span className="ml-auto flex shrink-0 items-center gap-1.5 text-[10px] font-medium">
          <span className="inline-flex items-center gap-1 rounded bg-emerald-500/15 px-1.5 py-0.5 text-emerald-600 dark:text-emerald-400">
            <Plus aria-hidden="true" className="h-3 w-3" />{adds}
          </span>
          <span className="inline-flex items-center gap-1 rounded bg-red-500/15 px-1.5 py-0.5 text-red-600 dark:text-red-400">
            <Minus aria-hidden="true" className="h-3 w-3" />{dels}
          </span>
        </span>
      </div>
      {useSideBySide ? (
        <SideBySideBody rows={rows} />
      ) : (
        <UnifiedBody rows={rows} />
      )}
    </div>
  );
}
