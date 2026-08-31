import { useState } from "react";
import { TerminalSquare } from "lucide-react";
import type { ParsedTool } from "@/components/executions/sessionItems";

interface ToolCardProps {
  tool: ParsedTool;
  className?: string;
}

export function ToolCard({ tool, className }: ToolCardProps) {
  const [open, setOpen] = useState(false);
  const hasOutput = tool.output.length > 0;
  const [copyState, setCopyState] = useState<"" | "input" | "output">("");

  const copy = (kind: "input" | "output") => {
    navigator.clipboard
      ?.writeText(kind === "input" ? tool.input : tool.output)
      .catch(() => {});
    setCopyState(kind);
    window.setTimeout(() => setCopyState(""), 1200);
  };

  return (
    <div className={`flex justify-start pl-2 ${className ?? ""}`}>
      <div className="w-full max-w-[92%] overflow-hidden rounded-xl border border-border/70 bg-muted/30">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="flex w-full items-center gap-2 px-3 py-2 text-left"
        >
          <TerminalSquare className="h-4 w-4 shrink-0 text-muted-foreground" />
          <span className="font-mono text-sm font-medium">{tool.toolName}</span>
          {hasOutput && (
            <span className="ml-auto shrink-0 rounded bg-muted px-2 py-0.5 text-xs text-muted-foreground">
              {tool.output.length.toLocaleString()} bytes
            </span>
          )}
          <span className="text-xs text-muted-foreground/60">
            {open ? "hide" : "view"}
          </span>
        </button>
        {open && (
          <div className="space-y-2 border-t border-border/50 p-3">
            <div>
              <div className="mb-1 flex items-center gap-2">
                <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                  input
                </span>
                <button
                  type="button"
                  onClick={() => copy("input")}
                  className="ml-auto text-xs text-muted-foreground hover:text-foreground"
                >
                  {copyState === "input" ? "copied" : "copy"}
                </button>
              </div>
              <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-words rounded bg-background/70 p-2 font-mono text-[13px] leading-relaxed">
                {tool.input || "—"}
              </pre>
            </div>
            {hasOutput && (
              <div>
                <div className="mb-1 flex items-center gap-2">
                  <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                    output
                  </span>
                  <button
                    type="button"
                    onClick={() => copy("output")}
                    className="ml-auto text-xs text-muted-foreground hover:text-foreground"
                  >
                    {copyState === "output" ? "copied" : "copy"}
                  </button>
                </div>
                <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded bg-background/70 p-2 font-mono text-[13px] leading-relaxed">
                  {tool.output}
                </pre>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
