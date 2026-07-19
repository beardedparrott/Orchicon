import { useMemo, useState } from "react";
import { type Edge, type Node } from "reactflow";

import type { StepData } from "./stepKinds";
import { canvasToYaml, yamlToCanvas } from "./workflowYaml";

interface CodeViewProps {
  nodes: Node<StepData>[];
  edges: Edge[];
  onUpdate: (nodes: Node<StepData>[], edges: Edge[]) => void;
}

export function CodeView({ nodes, edges, onUpdate }: CodeViewProps) {
  const initialYaml = useMemo(() => canvasToYaml(nodes, edges), [nodes, edges]);
  const [code, setCode] = useState(initialYaml);
  const [parseError, setParseError] = useState("");

  function apply() {
    const val = code.trim();
    if (!val) return;
    setParseError("");
    try {
      const result = yamlToCanvas(val);
      onUpdate(result.nodes, result.edges);
    } catch (err) {
      setParseError(String(err));
    }
  }

  function handleKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if ((e.metaKey || e.ctrlKey) && e.key === "s") {
      e.preventDefault();
      apply();
    }
  }

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center justify-between border-b px-4 py-2 text-xs text-muted-foreground">
        <span>YAML — Ctrl+S to apply</span>
        <span className="font-mono">{nodes.length} steps · {edges.length} connections</span>
      </div>
      {parseError && (
        <div className="border-b border-rose-300 bg-rose-50 px-4 py-1.5 text-xs text-rose-700 dark:border-rose-800 dark:bg-rose-950/40 dark:text-rose-300">
          {parseError}
        </div>
      )}
      <textarea
        className="flex-1 resize-none border-0 bg-transparent p-4 font-mono text-sm leading-relaxed outline-none"
        value={code}
        onChange={(e) => setCode(e.target.value)}
        onBlur={apply}
        onKeyDown={handleKeyDown}
        spellCheck={false}
      />
    </div>
  );
}
