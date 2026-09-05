// HeadsUpGrid — the tiled run view body. Renders one tile per DAG
// step in DAG (queue) order. PERF GUARD: at most ONE execution event
// stream is open at a time — `liveStepId` is the first step-run with
// status RUNNING (3) that has an execution; every other tile renders a
// static snapshot refreshed by the page-level workflow-event
// invalidation. While a tile is expanded (`suspended`), its stream
// unmounts so the modal owns the only live subscription.
import { HeadsUpTile } from "./HeadsUpTile";
import type { HeadsUpTileData } from "./headsUp";

interface HeadsUpGridProps {
  tiles: HeadsUpTileData[];
  runId: string;
  /** Step id whose stream is suspended (expanded into the modal). */
  suspendedStepId?: string | null;
  onExpand: (tile: HeadsUpTileData) => void;
}

export function HeadsUpGrid({ tiles, runId, suspendedStepId, onExpand }: HeadsUpGridProps) {
  const liveStepId = tiles.find(
    (t) => t.isActive && t.execution && t.stepId !== suspendedStepId,
  )?.stepId;
  if (tiles.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No steps in this workflow version yet.
      </p>
    );
  }
  return (
    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
      {tiles.map((t) => (
        <HeadsUpTile
          key={t.stepId}
          tile={t}
          runId={runId}
          liveStream={t.stepId === liveStepId}
          onExpand={onExpand}
        />
      ))}
    </div>
  );
}
