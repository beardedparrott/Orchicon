import { AlertTriangle, Loader2, Wrench } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";

import { useGetWorkflowRunDiagnosis, workflowKeys } from "@/api/workflows";
import { useReleaseStuckExecutions } from "@/api/executions";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

// RunBlockageCard — "why is this run not moving?", and what you can do about it.
//
// WHY THIS EXISTS. A run's blockage is usually RUN-EXTERNAL, and nothing in the
// run view could see it. Field incident: run 01M53DXZSF5G2T2Z592F7G5ETE sat
// "running" with an empty current step and no execution anywhere while the page
// said "waiting for dispatch" — because the PROJECT's dispatch cap was full,
// held by ten executions that were dead and never reaped. Every fact needed to
// understand that lived outside the run, so the operator was left guessing.
//
// The card reports; it does not decide. The one action it offers — releasing
// stuck executions — asks the liveness reaper to make the judgement, so this
// component can never kill an execution the probe considers alive.
//
// It renders NOTHING for a run that is not blocked. A diagnostic panel that
// appears on healthy runs is noise, and noise is what makes people stop reading
// the panel that matters.

function formatAge(seconds: number): string {
  if (!seconds || seconds <= 0) return "unknown age";
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86400)}d`;
}

export function RunBlockageCard({
  runId,
  /**
   * Whether the run is live. Terminal runs cannot block, so the query is off
   * for them and the card renders nothing.
   */
  enabled,
}: {
  runId: string;
  enabled: boolean;
}) {
  const qc = useQueryClient();
  const { data: diagnosis, isError } = useGetWorkflowRunDiagnosis(enabled ? runId : "", {
    pollMs: enabled ? 10000 : undefined,
  });
  const release = useReleaseStuckExecutions();

  // A failed diagnosis read is reported, not hidden. Silence is the defect this
  // whole feature exists to remove; a panel that vanishes on error teaches the
  // operator to trust its absence.
  if (isError) {
    return (
      <div className="rounded-xl border border-dashed p-3 text-xs text-muted-foreground">
        Could not read this run&rsquo;s diagnosis. The run may still be progressing — open the
        executions list to check.
      </div>
    );
  }

  if (!diagnosis || !diagnosis.blocked) return null;

  const stuck = diagnosis.stuckExecutions ?? [];
  const active = diagnosis.activeExecutions ?? 0;
  const limit = diagnosis.dispatchLimit ?? 0;

  const onRelease = () => {
    release.mutate(
      { projectId: diagnosis.projectId },
      {
        onSuccess: () => {
          // Releasing frees slots, which changes the run, its steps, and the
          // diagnosis itself — refetch all three rather than trusting the
          // optimistic view.
          qc.invalidateQueries({ queryKey: workflowKeys.runDiagnosis(runId) });
          qc.invalidateQueries({ queryKey: workflowKeys.run(runId) });
          qc.invalidateQueries({ queryKey: workflowKeys.stepRuns(runId) });
        },
      },
    );
  };

  const outcome = release.data;

  return (
    <div
      role="status"
      aria-live="polite"
      className="rounded-xl border border-amber-500/30 bg-amber-50 p-4 dark:bg-amber-950/20"
    >
      <div className="flex gap-3">
        <AlertTriangle className="h-5 w-5 shrink-0 text-amber-600" aria-hidden="true" />
        <div className="min-w-0 flex-1 space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <p className="text-sm font-semibold text-amber-900 dark:text-amber-100">
              This run is not progressing
            </p>
            <Badge variant="outline" className="font-mono text-[10px]">
              {diagnosis.blockageKind}
            </Badge>
          </div>

          <p className="text-sm text-amber-900/90 dark:text-amber-100/90">{diagnosis.detail}</p>

          {/* Capacity is shown only when a cap exists — "0/0" would read as a
              limit of zero rather than no limit at all. */}
          {limit > 0 && (
            <p className="text-xs text-amber-800 dark:text-amber-200">
              Project dispatch capacity:{" "}
              <span className="font-mono">
                {active}/{limit}
              </span>
              {diagnosis.atCapacity ? " — full" : ""}
            </p>
          )}

          {stuck.length > 0 && (
            <div className="space-y-1">
              <p className="text-xs font-medium text-amber-900 dark:text-amber-100">
                Holding slots while marked not-well:
              </p>
              <ul className="space-y-0.5">
                {stuck.map((e) => (
                  <li key={e.id} className="flex flex-wrap items-center gap-2 text-xs text-amber-800 dark:text-amber-200">
                    <span className="font-mono">{e.id.slice(0, 12)}…</span>
                    <span>{e.workerName || "unknown worker"}</span>
                    <Badge variant="outline" className="text-[10px]">
                      {e.status}
                    </Badge>
                    <span>{formatAge(Number(e.ageSeconds ?? 0))} old</span>
                  </li>
                ))}
              </ul>
            </div>
          )}

          {/* The action appears only when there is something to release. On a
              full cap with no stuck rows, the slots are held by LIVE work and
              the honest answer is "wait" — a button here would be a trap. */}
          {diagnosis.clearable && (
            <div className="flex flex-wrap items-center gap-2 pt-1">
              <Button
                variant="outline"
                size="sm"
                onClick={onRelease}
                disabled={release.isPending}
                aria-label={`Release ${stuck.length} stuck execution${stuck.length === 1 ? "" : "s"}`}
              >
                {release.isPending ? (
                  <Loader2 className="mr-1 h-3 w-3 animate-spin" />
                ) : (
                  <Wrench className="mr-1 h-3 w-3" />
                )}
                Release {stuck.length} stuck execution{stuck.length === 1 ? "" : "s"}
              </Button>
              <span className="text-xs text-amber-800/80 dark:text-amber-200/80">
                Only executions the liveness probe reports as gone are released — live work is
                never touched.
              </span>
            </div>
          )}

          {release.isError && (
            <p role="alert" className="text-xs text-red-700 dark:text-red-300">
              Release failed: {(release.error as Error).message}
            </p>
          )}

          {outcome && (
            <p className="text-xs text-amber-900 dark:text-amber-100">
              {outcome.releasedIds.length > 0
                ? `Released ${outcome.releasedIds.length} of ${outcome.examined} checked — the run can dispatch again.`
                : `Checked ${outcome.examined} execution(s); none were dead, so nothing was released. ` +
                  `The slots are held by live work and will free as it finishes.`}
            </p>
          )}
        </div>
      </div>
    </div>
  );
}
