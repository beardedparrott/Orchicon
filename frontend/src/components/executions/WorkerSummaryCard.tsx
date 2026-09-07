// WorkerSummaryCard — first-class at-a-glance verification that a worker
// passed its ORCHICON WORKER SUMMARY.
//
// The summary lives only as a text trailer inside the execution `Output`
// blob (convention `ORCHICON WORKER SUMMARY: <status> — <text>` plus
// optional `FACTS LEARNED:` lines). It is rendered ABOVE/OUTSIDE the
// session panes so it shows regardless of whether the event stream or the
// session transcript is present — explicitly covering the empty-conversation
// native-`orchicon`-bridge case where the summary would otherwise sit
// unseen in Output.
import { useState } from "react";
import { CheckCircle2, XCircle, AlertCircle, ChevronDown, ChevronRight } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { parseWorkerSummary, type WorkerSummary } from "./workerSummary";

interface WorkerSummaryCardProps {
  /** The execution's raw Output blob (may be empty/undefined). */
  output?: string | null;
  /** When true, render the loading placeholder instead of parsing. */
  loading?: boolean;
}

export function WorkerSummaryCard({ output, loading }: WorkerSummaryCardProps) {
  const summary = parseWorkerSummary(output);
  const [showRaw, setShowRaw] = useState(false);
  const [factsOpen, setFactsOpen] = useState(false);

  if (loading) {
    return (
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm">Worker summary</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground">Loading…</p>
        </CardContent>
      </Card>
    );
  }

  if (!summary.present) {
    return (
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm">Worker summary</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <AlertCircle aria-hidden="true" className="h-4 w-4 shrink-0" />
            <span>No worker summary recorded.</span>
          </div>
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader className="pb-2">
        <div className="flex items-center justify-between gap-2">
          <CardTitle className="flex items-center gap-2 text-sm">
            Worker summary
            <StatusPill status={summary.status} />
          </CardTitle>
          <button
            type="button"
            className="rounded px-1.5 py-0.5 text-xs font-medium text-muted-foreground hover:bg-accent hover:text-accent-foreground"
            onClick={() => setShowRaw((v) => !v)}
          >
            {showRaw ? "Render text" : "Raw"}
          </button>
        </div>
      </CardHeader>
      <CardContent className="space-y-2">
        {summary.text ? (
          <div className="break-words text-sm leading-relaxed [overflow-wrap:anywhere]">
            {showRaw ? (
              <pre className="whitespace-pre-wrap font-mono text-xs leading-relaxed">{summary.text}</pre>
            ) : (
              <span>{summary.text}</span>
            )}
          </div>
        ) : (
          <p className="text-sm italic text-muted-foreground">No summary text.</p>
        )}

        {summary.factsLearned.length > 0 && (
          <div className="rounded-lg border border-border/60 bg-muted/30">
            <button
              type="button"
              onClick={() => setFactsOpen((v) => !v)}
              className="flex w-full items-center gap-1.5 px-3 py-2 text-left text-xs font-medium text-muted-foreground hover:text-foreground"
            >
              {factsOpen ? (
                <ChevronDown aria-hidden="true" className="h-3.5 w-3.5" />
              ) : (
                <ChevronRight aria-hidden="true" className="h-3.5 w-3.5" />
              )}
              <span>
                FACTS LEARNED ({summary.factsLearned.length})
              </span>
            </button>
            {factsOpen && (
              <ul className="space-y-1 border-t border-border/50 px-3 py-2">
                {summary.factsLearned.map((fact, i) => (
                  <li key={i} className="break-words text-xs leading-relaxed text-muted-foreground [overflow-wrap:anywhere]">
                    {fact}
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function StatusPill({ status }: { status: WorkerSummary["status"] }) {
  const config = {
    success: {
      label: "success",
      icon: CheckCircle2,
      cls: "bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-300",
    },
    failure: {
      label: "failure",
      icon: XCircle,
      cls: "bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-300",
    },
    unknown: {
      label: "unknown",
      icon: AlertCircle,
      cls: "bg-muted text-muted-foreground",
    },
  }[status];
  const Icon = config.icon;
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[10px] font-medium",
        config.cls,
      )}
    >
      <Icon aria-hidden="true" className="h-3 w-3" />
      {config.label}
    </span>
  );
}
