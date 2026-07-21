import { useState } from "react";
import { createRoute, useNavigate } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";

import {
  useApproveContinuationPlan,
  useCancelRecovery,
  useGetContinuationPlan,
  useGetRecovery,
  useGetRecoveryStepRuns,
  useMarkTaskSucceeded,
  useRejectContinuationPlan,
} from "@/api/recovery";
import { useStreamRecoveryEvents } from "@/api/recoveryEvents";
import { Markdown } from "@/components/markdown";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Route as rootRoute } from "@/routes/__root";
import type { RecoveryEvent } from "@/api/gen/orchicon/api/v1/recovery_pb";
import { cn } from "@/lib/utils";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/recovery/$id",
  component: RecoveryDetailPage,
});

function RecoveryDetailPage() {
  const { id } = Route.useParams();
  const navigate = useNavigate();
  const { data: recovery, isLoading } = useGetRecovery(id);
  const { data: stepRuns } = useGetRecoveryStepRuns(id);
  const { data: plan } = useGetContinuationPlan(id);
  const cancelRecovery = useCancelRecovery();
  const approvePlan = useApproveContinuationPlan();
  const rejectPlan = useRejectContinuationPlan();
  const markSucceeded = useMarkTaskSucceeded();

  const [liveEvents, setLiveEvents] = useState<RecoveryEvent[]>([]);
  useStreamRecoveryEvents({
    recoveryId: id,
    onEvent: (evt) => setLiveEvents((prev) => [...prev.slice(-49), evt]),
  });

  const [actor, setActor] = useState("operator");
  const [reason, setReason] = useState("");

  if (isLoading || !recovery) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }

  const isBlocked = recovery.status === 7; // blocked (L3 awaiting approval)
  const isTerminal =
    recovery.status === 3 || recovery.status === 5 || recovery.status === 6; // resumed/failed/cancelled

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex min-w-0 items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/recovery" })}
            className="shrink-0"
          >
            <ArrowLeft className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <div className="min-w-0">
            <h1 className="text-lg font-semibold tracking-tight sm:text-2xl">
              Recovery{" "}
              <span className="font-mono text-base">{recovery.id.slice(0, 16)}…</span>
            </h1>
            <p className="truncate text-sm text-muted-foreground">
              task{" "}
              <span className="font-mono">{recovery.taskId.slice(0, 12)}…</span>{" "}
              · L{recovery.level} · {recovery.resumptionPath}
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {!isTerminal && (
            <Button
              variant="outline"
              onClick={() => cancelRecovery.mutate({ recoveryId: id, reason })}
            >
              Cancel recovery
            </Button>
          )}
        </div>
      </div>

      {/* Recovery summary — the narrative header */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-3">
            <span>What went wrong</span>
            <span
              className={cn(
                "rounded-full px-2 py-0.5 text-xs font-medium",
                STATUS_STYLES[recovery.status],
              )}
            >
              {STATUS_LABELS[recovery.status]}
            </span>
          </CardTitle>
          <CardDescription>
            The trigger that started this recovery (why), the affected
            execution (what), the resumption path chosen (how).
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4 text-sm md:grid-cols-2">
          <div>
            <span className="text-muted-foreground">Trigger (why): </span>
            <span className="font-medium">{recovery.triggerReason}</span>
          </div>
          <div>
            <span className="text-muted-foreground">Failed execution (what): </span>
            <span className="font-mono text-xs">
              {recovery.failedExecutionId.slice(0, 16)}…
            </span>
          </div>
          <div>
            <span className="text-muted-foreground">Resumption (how): </span>
            <span className="font-medium">{recovery.resumptionPath}</span>
          </div>
          <div>
            <span className="text-muted-foreground">Budget relax: </span>
            <span className="font-medium">
              {((recovery.budgetRelaxFraction || 0) * 100).toFixed(0)}%
              {recovery.needsHumanApproval && (
                <span className="ml-1 text-orange-600">
                  · human approval required
                </span>
              )}
            </span>
          </div>
          {recovery.summary && (
            <div className="md:col-span-2">
              <span className="text-muted-foreground">Summary: </span>
              <Markdown>{recovery.summary}</Markdown>
            </div>
          )}
        </CardContent>
      </Card>

      <div className="grid gap-6 lg:grid-cols-2">
        {/* Step timeline — the full narrative */}
        <Card>
          <CardHeader>
            <CardTitle>Recovery arc</CardTitle>
            <CardDescription>
              The 6-step workflow (capture → summarize → preserve → review →
              plan → resume). Each step carries why/what/how/where/when
              (docs/06 §11).
            </CardDescription>
          </CardHeader>
          <CardContent>
            {!stepRuns || stepRuns.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No steps yet — recovery is starting.
              </p>
            ) : (
              <ol className="relative space-y-4 border-l pl-6">
                {stepRuns.map((sr) => (
                  <li key={sr.id} className="relative">
                    <span
                      className={cn(
                        "absolute -left-[1.65rem] flex h-4 w-4 items-center justify-center rounded-full text-[8px] font-bold",
                        STEP_DOT[sr.status] ?? "bg-muted",
                      )}
                    />
                    <div className="flex items-center justify-between">
                      <span className="font-medium">{sr.stepName}</span>
                      <span
                        className={cn(
                          "rounded-full px-2 py-0.5 text-xs",
                          STEP_STATUS_STYLES[sr.status],
                        )}
                      >
                        {STEP_STATUS_LABELS[sr.status]}
                      </span>
                    </div>
                    {sr.action && (
                      <p className="text-xs text-muted-foreground">
                        <span className="text-foreground/70">how:</span>{" "}
                        {sr.action}
                      </p>
                    )}
                    {sr.triggerReason && (
                      <p className="text-xs text-muted-foreground">
                        <span className="text-foreground/70">why:</span>{" "}
                        {sr.triggerReason}
                      </p>
                    )}
                    {sr.affectedRef && (
                      <p className="text-xs text-muted-foreground">
                        <span className="text-foreground/70">what:</span>{" "}
                        <span className="font-mono">
                          {sr.affectedRef.slice(0, 16)}…
                        </span>
                      </p>
                    )}
                    {sr.adapterRef && (
                      <p className="text-xs text-muted-foreground">
                        <span className="text-foreground/70">where:</span>{" "}
                        {sr.adapterRef}
                      </p>
                    )}
                    {sr.startedAt && (
                      <p className="text-xs text-muted-foreground">
                        <span className="text-foreground/70">when:</span>{" "}
                        {fmtTime(sr.startedAt)} → {sr.endedAt ? fmtTime(sr.endedAt) : "…"}
                      </p>
                    )}
                    {sr.result && sr.result !== "{}" && (
                      <details>
                        <summary className="cursor-pointer text-xs text-muted-foreground">
                          result
                        </summary>
                        <div className="mt-1 max-h-40 overflow-auto rounded bg-muted/30 p-2">
                          <Markdown>{sr.result}</Markdown>
                        </div>
                      </details>
                    )}
                  </li>
                ))}
              </ol>
            )}
          </CardContent>
        </Card>

        <div className="space-y-6">
          {/* Continuation plan */}
          <Card>
            <CardHeader>
              <CardTitle>Continuation plan</CardTitle>
              <CardDescription>
                Produced by the plan step (docs/06 §8). L3 escalation
                requires human approval before resume.
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-4">
              {!plan ? (
                <p className="text-sm text-muted-foreground">
                  No plan produced yet.
                </p>
              ) : (
                <>
                  <div className="flex items-center justify-between text-sm">
                    <span>
                      status:{" "}
                      <span
                        className={cn(
                          "rounded-full px-2 py-0.5 text-xs",
                          PLAN_STATUS_STYLES[plan.status],
                        )}
                      >
                        {PLAN_STATUS_LABELS[plan.status]}
                      </span>
                    </span>
                    {plan.decidedAt && (
                      <span className="text-xs text-muted-foreground">
                        by {plan.approvedBy || "—"}
                      </span>
                    )}
                  </div>
                  {plan.contextSummary && (
                    <div>
                      <Label className="text-xs">Context summary</Label>
                      <div className="text-sm">
                        <Markdown>{plan.contextSummary}</Markdown>
                      </div>
                    </div>
                  )}
                  {plan.remaining && plan.remaining !== "[]" && (
                    <details>
                      <summary className="cursor-pointer text-xs text-muted-foreground">
                        Remaining work
                      </summary>
                      <pre className="mt-1 overflow-auto font-mono text-[10px]">
                        {plan.remaining}
                      </pre>
                    </details>
                  )}
                  {plan.checkpointRef && (
                    <p className="text-xs text-muted-foreground">
                      checkpoint:{" "}
                      <span className="font-mono">{plan.checkpointRef}</span>
                    </p>
                  )}
                  {isBlocked && plan.status === 1 && (
                    <div className="space-y-2 rounded-md border border-orange-300 bg-orange-50 p-3">
                      <p className="text-sm font-medium text-orange-800">
                        Human approval required (L3)
                      </p>
                      <Input
                        value={actor}
                        onChange={(e) => setActor(e.target.value)}
                        placeholder="approver identity"
                      />
                      <Textarea
                        rows={2}
                        value={reason}
                        onChange={(e) => setReason(e.target.value)}
                        placeholder="reason (optional)"
                      />
                      <div className="flex gap-2">
                        <Button
                          size="sm"
                          onClick={() =>
                            approvePlan.mutate({ recoveryId: id, actor })
                          }
                          disabled={approvePlan.isPending}
                        >
                          {approvePlan.isPending ? "Approving…" : "Approve"}
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() =>
                            rejectPlan.mutate({
                              recoveryId: id,
                              actor,
                              reason,
                            })
                          }
                          disabled={rejectPlan.isPending}
                        >
                          Reject
                        </Button>
                      </div>
                    </div>
                  )}
                </>
              )}
            </CardContent>
          </Card>

          {/* Task completion (Reviewer / human) */}
          <Card>
            <CardHeader>
              <CardTitle>Mark task succeeded</CardTitle>
              <CardDescription>
                The Reviewer Worker (during recovery) or a human can mark
                the Task succeeded (docs/06 §11, docs/02 §4 #2). Emits an
                audit event with the actor recorded.
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-2">
              <Input
                value={actor}
                onChange={(e) => setActor(e.target.value)}
                placeholder="actor id"
              />
              <Input
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                placeholder="why the task is complete"
              />
              <Button
                size="sm"
                variant="outline"
                onClick={() =>
                  markSucceeded.mutate({
                    taskId: recovery.taskId,
                    actorType: "human",
                    actorId: actor,
                    reason,
                  })
                }
              >
                Mark succeeded
              </Button>
            </CardContent>
          </Card>

          {/* Live event feed */}
          <Card>
            <CardHeader>
              <CardTitle>Live events</CardTitle>
              <CardDescription>
                Streaming recovery events (StreamRecoveryEvents — docs/07
                §3.6).
              </CardDescription>
            </CardHeader>
            <CardContent>
              {liveEvents.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  Waiting for events…
                </p>
              ) : (
                <ul className="space-y-1 text-xs">
                  {liveEvents.map((e, i) => (
                    <li key={i} className="font-mono">
                      <span className="text-muted-foreground">
                        {e.eventType}
                      </span>{" "}
                      {e.action && (
                        <span className="text-foreground/70">· {e.action}</span>
                      )}
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  );
}

function fmtTime(ts: { seconds?: bigint; nanos?: number } | number | string | undefined | null): string {
  if (!ts) return "—";
  try {
    let d: Date;
    if (typeof ts === "number" || typeof ts === "string") {
      d = new Date(ts);
    } else if (typeof ts === "object" && ts !== null && ts.seconds !== undefined) {
      d = new Date(Number(ts.seconds) * 1000);
    } else {
      return "—";
    }
    return d.toLocaleTimeString();
  } catch {
    return "—";
  }
}

const STATUS_LABELS: Record<number, string> = {
  1: "pending",
  2: "running",
  3: "resumed",
  4: "escalated",
  5: "failed",
  6: "cancelled",
  7: "blocked",
};
const STATUS_STYLES: Record<number, string> = {
  1: "bg-blue-100 text-blue-800",
  2: "bg-indigo-100 text-indigo-800",
  3: "bg-green-100 text-green-800",
  4: "bg-yellow-100 text-yellow-800",
  5: "bg-red-100 text-red-800",
  6: "bg-muted text-muted-foreground",
  7: "bg-orange-100 text-orange-800",
};

const STEP_STATUS_LABELS: Record<number, string> = {
  1: "pending",
  2: "ready",
  3: "running",
  4: "succeeded",
  5: "failed",
  6: "skipped",
  7: "blocked",
};
const STEP_STATUS_STYLES: Record<number, string> = {
  1: "bg-blue-100 text-blue-800",
  2: "bg-cyan-100 text-cyan-800",
  3: "bg-indigo-100 text-indigo-800",
  4: "bg-green-100 text-green-800",
  5: "bg-red-100 text-red-800",
  6: "bg-muted text-muted-foreground",
  7: "bg-orange-100 text-orange-800",
};
const STEP_DOT: Record<number, string> = {
  3: "bg-indigo-500 text-white",
  4: "bg-green-500 text-white",
  5: "bg-red-500 text-white",
  6: "bg-muted",
  7: "bg-orange-500 text-white",
};

const PLAN_STATUS_LABELS: Record<number, string> = {
  1: "pending",
  2: "approved",
  3: "rejected",
};
const PLAN_STATUS_STYLES: Record<number, string> = {
  1: "bg-orange-100 text-orange-800",
  2: "bg-green-100 text-green-800",
  3: "bg-red-100 text-red-800",
};
