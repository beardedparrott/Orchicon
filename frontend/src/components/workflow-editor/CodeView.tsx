import { type Edge, type Node } from "reactflow";

import type { StepData } from "./stepKinds";
import {
  canvasToYaml,
  yamlToCanvas,
} from "./workflowYaml";

interface CodeViewProps {
  nodes: Node<StepData>[];
  edges: Edge[];
  onUpdate: (nodes: Node<StepData>[], edges: Edge[]) => void;
}

export function CodeView({ nodes, edges, onUpdate }: CodeViewProps) {
  const yaml = canvasToYaml(nodes, edges);

  function handleBlur(e: React.FocusEvent<HTMLTextAreaElement>) {
    const val = e.currentTarget.value.trim();
    if (!val) return;
    try {
      const result = yamlToCanvas(val);
      onUpdate(result.nodes, result.edges);
    } catch {
      // Error is shown in-place; don't clobber the canvas
    }
  }

  return (
    <div className="flex flex-1 flex-col">
      <div className="flex items-center justify-between border-b px-4 py-2 text-xs text-muted-foreground">
        <span>YAML — edits apply when the textarea loses focus</span>
        <span className="font-mono">{nodes.length} steps · {edges.length} connections</span>
      </div>
      <textarea
        className="flex-1 resize-none border-0 bg-transparent p-4 font-mono text-sm leading-relaxed outline-none"
        defaultValue={yaml}
        onBlur={handleBlur}
        spellCheck={false}
      />
    </div>
  );
}
