import { OctagonAlert } from "lucide-react";

import { Badge } from "@/components/ui/badge";

// RunFailureCard — WHY THIS RUN FAILED, on the run page, without decoding anything.
//
// REPORTED: a run failed 3.8s after starting with "could not create the run worktree: … fatal: invalid
// reference: HEAD". Every operator surface was silent — the work item's acceptance review read "no summary
// recorded", and diagnose_failure returned {"failure_analysis": [], "workflow_executions": null}. The reason
// existed the whole time, inside run_context, which the API ships base64-encoded.
//
// WHY A SEPARATE CARD rather than the blockage card: a blockage card answers "why is this run not MOVING" and
// only a running run can be blocked. A failed run is finished, so that card renders nothing — correctly. What
// it never had was a place to say why it ENDED.
//
// THE KIND IS SHOWN because it is the difference between "our git preflight refused" and "the worker's code
// broke" — two problems with two different owners. It is rendered as the raw vocabulary rather than prose on
// purpose: the message above it is already phrased for a person, and inventing a second friendly label per
// kind would let the two drift.
export function RunFailureCard({
  errorMessage,
  errorKind,
}: {
  errorMessage?: string;
  errorKind?: string;
}) {
  // Nothing recorded is NOT the same as nothing wrong, but this card can only
  // report what was recorded. Runs that failed before the field existed fall
  // here, and rendering an empty card would be noise.
  if (!errorMessage) return null;

  return (
    <div
      role="alert"
      className="rounded-xl border border-destructive/40 bg-destructive/5 p-4"
    >
      <div className="flex gap-3">
        <OctagonAlert className="h-5 w-5 shrink-0 text-destructive" aria-hidden="true" />
        <div className="min-w-0 flex-1 space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <p className="text-sm font-semibold text-destructive">This run failed</p>
            {errorKind && (
              <Badge variant="outline" className="font-mono text-[10px]">
                {errorKind}
              </Badge>
            )}
          </div>
          {/* whitespace-pre-wrap: the platform writes a led remedy followed by a blank
              line and the raw detail, and collapsing that newline runs the two
              together into one sentence. */}
          <p className="whitespace-pre-wrap break-words text-sm text-foreground/90">{errorMessage}</p>
        </div>
      </div>
    </div>
  );
}
