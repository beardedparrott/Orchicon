import { useState } from "react";
import { TerminalSquare } from "lucide-react";
import type { ParsedTool } from "@/components/executions/sessionItems";
import { collapseWhitespace, isWebTool, rowSubject } from "@/lib/tool-row-subject";

interface ToolCardProps {
  tool: ParsedTool;
  className?: string;
}

export function ToolCard({ tool, className }: ToolCardProps) {
  const [open, setOpen] = useState(false);
  const hasOutput = tool.output.length > 0;
  // A FAILED CALL'S REASON IS DRAWN IN FULL, OUTSIDE THE TOGGLE. Truncating output to a byte count and a
  // `view` link is right for a normal result (the row is a summary; the detail is one click away), and it
  // is wrong for a refusal: when the platform declines to run a tool, that text IS the message — why it
  // did not run and what the operator must change. Collapsed, the transcript says "write_file" and shows
  // the operator nothing they can act on. The TUI's renderToolRow/toolRowError draws the same body for the
  // same reason (internal/tui/chat/view.go).
  const refusal = tool.failed ? tool.output.trim() : "";
  const hasRefusal = refusal.length > 0;
  // THE SUBJECT, not the raw input JSON. rowSubject reads BOTH argument vocabularies (opencode's
  // `command`/`file_path` AND agy's `CommandLine`/`TargetFile`/`Url`/`query`), so an antigravity tool call
  // shows its command, path or URL on the row instead of a truncated JSON object. The TUI draws the same
  // line from the same fixture (internal/toolclass/row_subject.go).
  const subject = collapseWhitespace(rowSubject(tool.toolName, tool.input));
  const web = isWebTool(tool.toolName);
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
          <TerminalSquare aria-hidden="true" className="h-4 w-4 shrink-0 text-muted-foreground" />
          <span className="font-mono text-sm font-medium">{tool.toolName}</span>
          {subject && (
            <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground">
              {web ? "→ " : ""}
              {subject}
            </span>
          )}
          {hasOutput && (
            <span className="ml-auto shrink-0 rounded bg-muted px-2 py-0.5 text-xs text-muted-foreground">
              {tool.output.length.toLocaleString()} bytes
            </span>
          )}
          <span className="text-xs text-muted-foreground/60">
            {open ? "hide" : "view"}
          </span>
        </button>
        {/* THE REFUSAL, IN FULL AND ALWAYS VISIBLE. It is the row's meaning, not its detail, so it is not
            behind the toggle: an operator reading the transcript has to be able to see that the call was
            declined and why without clicking. Styled with the destructive token the transcript already
            uses for an error (ErrorBubble), so a refused row reads as one of the same family. */}
        {hasRefusal && (
          <div
            data-testid="tool-refusal"
            className="whitespace-pre-wrap break-words border-t border-destructive/40 bg-destructive/10 px-3 py-2 font-mono text-xs leading-relaxed text-destructive"
          >
            {refusal}
          </div>
        )}
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
            {hasOutput && !hasRefusal && (
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
