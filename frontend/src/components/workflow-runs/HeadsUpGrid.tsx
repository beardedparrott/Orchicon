// HeadsUpGrid — the tiled run view body. Renders one tile per DAG
// step in DAG (queue) order. PERF GUARD: at most ONE execution event
// stream is open at a time — `liveStepId` is the first step-run with
// status RUNNING (3) that has an execution; every other tile renders a
// static snapshot refreshed by the page-level workflow-event
// invalidation. While a tile is expanded (`suspended`), its stream
// unmounts so the modal owns the only live subscription.
import { useState } from "react";

import { HeadsUpTile } from "./HeadsUpTile";
import { HeadsUpExpandedModal } from "./HeadsUpExpandedModal";
import type { HeadsUpTileData } from "./headsUp";

interface HeadsUpGridProps {
  tiles: HeadsUpTileData[];
  runId: string;
  /** Step id whose stream is suspended (expanded into the modal). */
  /** Step id whose quick-look modal is open — AT MOST ONE, and only while open. */
  expandedStepId?: string;
  onExpandedChange?: (stepId: string | undefined) => void;
}

export function HeadsUpGrid({ tiles, runId, expandedStepId, onExpandedChange }: HeadsUpGridProps) {
  // ONE modal, owned here. The grid stays static; only the open modal is live, and it unmounts the
  // moment it closes — releasing its socket rather than leaving it behind the next page.
  const [localExpanded, setLocalExpanded] = useState<string | undefined>(undefined);
  const expanded = expandedStepId !== undefined ? expandedStepId : localExpanded;
  const setExpanded = onExpandedChange ?? setLocalExpanded;
  const expandedTile = tiles.find((t) => t.stepId === expanded);
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
    <>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
        {tiles.map((t) => (
          <HeadsUpTile
            key={t.stepId}
            tile={t}
            runId={runId}
            onExpand={t.execution?.id ? () => setExpanded(t.stepId) : undefined}
          />
        ))}
      </div>
      {/* ONE live view, only while open. Rendering it conditionally is what makes close == release. */}
      {expandedTile?.execution?.id ? (
        <HeadsUpExpandedModal
          tile={expandedTile}
          onClose={() => setExpanded(undefined)}
        />
      ) : null}
    </>
  );
}
