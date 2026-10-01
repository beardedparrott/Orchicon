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
}

export function HeadsUpGrid({ tiles, runId }: HeadsUpGridProps) {
  // NOTHING IN THE GRID IS LIVE, so there is no "live step" to pick. A tile is a
  // thumbnail: it shows the step's status and last known summary and holds no stream and no
  // timer. The EXPANDED tile and the execution page own liveness for one execution each —
  // which is what this view should always have done, because the alternative was N timers
  // plus a stream running behind a grid of cards the operator is only scanning.
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
        />
      ))}
    </div>
  );
}
