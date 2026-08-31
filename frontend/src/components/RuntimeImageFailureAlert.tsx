import { useState } from "react";
import { AlertTriangle, Copy, ChevronDown, ChevronUp } from "lucide-react";

import { Button } from "@/components/ui/button";

export function RuntimeImageFailureAlert({
  reason,
  failedStep,
  logTail,
  buildLog,
  category,
}: {
  reason: string;
  failedStep?: string;
  logTail?: string;
  buildLog?: string;
  category?: string;
}) {
  const [open, setOpen] = useState(false);
  const copy = async () => {
    const text = [reason, failedStep ? `Step: ${failedStep}` : "", logTail || buildLog || ""].filter(Boolean).join("\n");
    try { await navigator.clipboard.writeText(text); } catch {}
  };
  if (!reason) return null;
  return (
    <div role="alert" className="rounded-xl border border-amber-500/30 bg-amber-50 dark:bg-amber-950/20 p-4">
      <div className="flex gap-3">
        <AlertTriangle className="h-5 w-5 shrink-0 text-amber-600" aria-hidden="true" />
        <div className="min-w-0 flex-1 space-y-1">
          <p className="text-sm font-semibold text-amber-900 dark:text-amber-100">{category ? `[${category}] ${reason}` : reason}</p>
          {failedStep && <p className="text-xs text-amber-800 dark:text-amber-200">Failed step: <code className="rounded bg-amber-100 px-1 dark:bg-amber-900">{failedStep}</code></p>}
          <div className="flex gap-2">
            <Button variant="outline" size="sm" onClick={() => setOpen(!open)} aria-expanded={open}>
              {open ? <ChevronUp className="mr-1 h-3 w-3" /> : <ChevronDown className="mr-1 h-3 w-3" />}
              {open ? "Hide log" : "Show log"}
            </Button>
            <Button variant="outline" size="sm" onClick={copy} aria-label="Copy failure details">
              <Copy className="mr-1 h-3 w-3" /> Copy
            </Button>
          </div>
          {open && (
            <pre className="mt-2 max-h-64 overflow-y-auto whitespace-pre-wrap break-words rounded-md border bg-zinc-950 p-2 font-mono text-xs text-zinc-100">
              {logTail || buildLog || "No log available"}
            </pre>
          )}
        </div>
      </div>
    </div>
  );
}
