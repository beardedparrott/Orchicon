// Report outcomes + models — the two questions the cost table cannot answer.
//
// A cost/window table tells an operator what work consumed; it does not tell
// them whether any of it LANDED. These two sections answer that: what finished
// and how it ended (the outcome counters), and which models did the work (the
// leaderboard and its statistics).
//
// Both read the same GetTelemetryReport response as the table beside them, so
// they keep working while a telemetry store is down — that is the Reports
// view's whole contract (Postgres only).
//
// The counters are NOT summed into one "throughput" number on purpose. Work
// items and workflow runs are different units, and a run of a workflow can be
// many executions: adding them would produce a total that means nothing.

import type {
  TelemetryReportModel,
  TelemetryReportOutcomes,
} from "@/api/gen/orchicon/api/v1/telemetry_service_pb";
import { BarList } from "@/components/ui/chart";
import { fmtInt } from "@/components/telemetry/states";
import { formatCompact } from "@/lib/chart-geometry";

interface OutcomeLine {
  key: string;
  label: string;
  value: number;
  /** Muted detail (e.g. "never ran") for a state that needs explaining. */
  hint?: string;
}

/** OutcomeCard is one column of the "what finished" roll-up. */
function OutcomeCard({
  title,
  caption,
  lines,
  empty,
}: {
  title: string;
  caption: string;
  lines: OutcomeLine[];
  empty: boolean;
}) {
  return (
    <div className="rounded-xl glass-panel p-4">
      <h3 className="text-sm font-medium">{title}</h3>
      <p className="mb-2 text-[11px] text-muted-foreground">{caption}</p>
      {empty ? (
        <p className="py-2 text-sm text-muted-foreground">
          Nothing finished in this window.
        </p>
      ) : (
        <dl className="space-y-1">
          {lines.map((l) => (
            <div
              key={l.key}
              className="flex items-baseline justify-between gap-2 text-sm"
            >
              <dt className="text-muted-foreground">
                {l.label}
                {l.hint ? (
                  <span className="ml-1 text-[11px] opacity-70">
                    ({l.hint})
                  </span>
                ) : null}
              </dt>
              <dd className="tabular-nums">{fmtInt(l.value)}</dd>
            </div>
          ))}
        </dl>
      )}
    </div>
  );
}

export function ReportOutcomes({
  outcomes,
}: {
  outcomes?: TelemetryReportOutcomes;
}) {
  const workFinished =
    (outcomes?.workSucceeded ?? 0) +
    (outcomes?.workSkipped ?? 0) +
    (outcomes?.workCancelled ?? 0) +
    (outcomes?.workFailed ?? 0);
  const runsFinished =
    (outcomes?.workflowsCompleted ?? 0) +
    (outcomes?.workflowsFailed ?? 0) +
    (outcomes?.workflowsAborted ?? 0);

  // Skipped and aborted only appear when they happened: a permanent "0
  // skipped" row is noise, and its presence implies it is a state you should
  // be watching for.
  const workLines: OutcomeLine[] = [
    {
      key: "succeeded",
      label: "Completed",
      value: outcomes?.workSucceeded ?? 0,
    },
    ...((outcomes?.workSkipped ?? 0) > 0
      ? [
          {
            key: "skipped",
            label: "Skipped",
            value: outcomes?.workSkipped ?? 0,
            hint: "never ran",
          },
        ]
      : []),
    { key: "failed", label: "Failed", value: outcomes?.workFailed ?? 0 },
    {
      key: "cancelled",
      label: "Cancelled",
      value: outcomes?.workCancelled ?? 0,
    },
    {
      key: "open",
      label: "Still open",
      value: outcomes?.workOpen ?? 0,
      hint: "in flight",
    },
  ];

  const runLines: OutcomeLine[] = [
    {
      key: "completed",
      label: "Completed",
      value: outcomes?.workflowsCompleted ?? 0,
    },
    { key: "failed", label: "Failed", value: outcomes?.workflowsFailed ?? 0 },
    ...((outcomes?.workflowsAborted ?? 0) > 0
      ? [
          {
            key: "aborted",
            label: "Aborted",
            value: outcomes?.workflowsAborted ?? 0,
          },
        ]
      : []),
    {
      key: "running",
      label: "Running",
      value: outcomes?.workflowsRunning ?? 0,
    },
  ];

  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <OutcomeCard
        title="Work items"
        caption="Counted when they reached a terminal state inside the window."
        lines={workLines}
        empty={workFinished === 0 && (outcomes?.workOpen ?? 0) === 0}
      />
      <OutcomeCard
        title="Workflow runs"
        caption="Completed is the success state; aborted is cancelled, not failed."
        lines={runLines}
        empty={runsFinished === 0 && (outcomes?.workflowsRunning ?? 0) === 0}
      />
    </div>
  );
}

export function ReportModels({ models }: { models: TelemetryReportModel[] }) {
  if (models.length === 0) {
    return (
      <div className="rounded-xl glass-panel p-4">
        <h3 className="mb-2 text-sm font-medium">Models</h3>
        <p className="text-sm text-muted-foreground">
          No model usage recorded in this window.
        </p>
      </div>
    );
  }

  const totalCost = models.reduce((sum, m) => sum + m.costUsd, 0);

  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <div className="rounded-xl glass-panel p-4">
        <h3 className="mb-2 text-sm font-medium">Top models</h3>
        <BarList
          items={models.slice(0, 8).map((m) => ({
            key: m.model,
            label: m.model,
            value: m.costUsd,
            note: `${formatCompact(Number(m.totalTokens))} tok`,
          }))}
          valueFormat={(v) => `$${v.toFixed(4)}`}
        />
      </div>
      <div className="overflow-x-auto rounded-xl glass-panel p-4">
        <h3 className="mb-2 text-sm font-medium">Model statistics</h3>
        <table className="w-full text-left text-sm">
          <caption className="sr-only">
            Model statistics for the report window: tokens, share of cost,
            executions and average cost per execution. Ordered by cost, highest
            first.
          </caption>
          <thead>
            <tr className="text-[11px] uppercase text-muted-foreground">
              <th scope="col" className="py-1 pr-2 font-medium">
                Model
              </th>
              <th scope="col" className="py-1 pr-2 font-medium">
                Tokens
              </th>
              <th scope="col" className="py-1 pr-2 font-medium">
                Cost
              </th>
              <th scope="col" className="py-1 pr-2 font-medium">
                Share
              </th>
              <th scope="col" className="py-1 font-medium">
                Execs
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-white/10">
            {models.map((m) => (
              <tr key={m.model}>
                <td className="py-1.5 pr-2">
                  <span className="line-clamp-1" title={m.model}>
                    {m.model}
                  </span>
                </td>
                <td className="py-1.5 pr-2 tabular-nums">
                  {formatCompact(Number(m.totalTokens))}
                </td>
                <td className="py-1.5 pr-2 tabular-nums">
                  ${m.costUsd.toFixed(4)}
                </td>
                <td className="py-1.5 pr-2 tabular-nums">
                  {totalCost > 0
                    ? `${((m.costUsd / totalCost) * 100).toFixed(1)}%`
                    : "—"}
                </td>
                <td className="py-1.5 tabular-nums">{fmtInt(m.executions)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
