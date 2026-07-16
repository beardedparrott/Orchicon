// Custom React Flow edge with a hover-only × delete handle at the
// midpoint. Pressing the × removes the edge via onEdgesChange.
//
// Built on top of React Flow's default BezierEdge (the visual is
// identical to a regular edge when the handle is not hovered). The
// delete handle is rendered via React Flow's EdgeLabelRenderer so it
// sits on top of the SVG path regardless of zoom/pan.
//
// PR D: the affordance replaces the older "press Del/Backspace on a
// selected edge" pattern. The keyboard shortcut still works for power
// users; this just makes the action discoverable on hover.

import { useState } from "react";
import {
  BaseEdge,
  EdgeLabelRenderer,
  getBezierPath,
  type EdgeProps,
} from "reactflow";
import { X } from "lucide-react";

export function DeletableEdge({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  sourcePosition,
  targetPosition,
  style,
  markerEnd,
  selected,
}: EdgeProps) {
  const [edgePath, labelX, labelY] = getBezierPath({
    sourceX,
    sourceY,
    sourcePosition,
    targetX,
    targetY,
    targetPosition,
  });
  const [hovered, setHovered] = useState(false);
  const showHandle = hovered || !!selected;
  return (
    <>
      <BaseEdge path={edgePath} markerEnd={markerEnd} style={style} />
      <EdgeLabelRenderer>
        <div
          style={{
            position: "absolute",
            transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)`,
            pointerEvents: "all",
          }}
          className="nodrag nopan"
          onMouseEnter={() => setHovered(true)}
          onMouseLeave={() => setHovered(false)}
          data-edge-id={id}
        >
          <button
            type="button"
            aria-label="Delete edge"
            title="Delete edge"
            // Surface the handle on hover or when the edge is selected.
            className={
              "flex h-5 w-5 items-center justify-center rounded-full border bg-background text-muted-foreground shadow-sm transition hover:bg-rose-100 hover:text-rose-700 dark:hover:bg-rose-950/60 " +
              (showHandle ? "opacity-100" : "pointer-events-none opacity-0")
            }
            onClick={(e) => {
              e.stopPropagation();
              // Dispatch a window event the parent listens for. Keeps
              // the single-source-of-truth state model.
              window.dispatchEvent(
                new CustomEvent("orchicon:delete-edge", { detail: { id } }),
              );
            }}
          >
            <X className="h-3 w-3" />
          </button>
        </div>
      </EdgeLabelRenderer>
    </>
  );
}
