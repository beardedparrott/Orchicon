import { useState } from "react";
import { createRoute, useNavigate } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";

import {
  useEvaluatePolicy,
  useGetPolicy,
  useListDecisions,
  useListPolicyVersions,
  usePublishPolicy,
  useSupersedePolicy,
  useUpdatePolicyVersion,
} from "@/api/policies";
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
import {
  DecisionPoint,
} from "@/api/gen/orchicon/api/v1/policy_pb";
import { cn } from "@/lib/utils";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/policies/$id",
  component: PolicyDetailPage,
});

function PolicyDetailPage() {
  const { id } = Route.useParams();
  const navigate = useNavigate();
  const { data, isLoading } = useGetPolicy(id);
  const { data: versions } = useListPolicyVersions(id);
  const updateVersion = useUpdatePolicyVersion();
  const publishPolicy = usePublishPolicy();
  const supersedePolicy = useSupersedePolicy();
  const evaluatePolicy = useEvaluatePolicy();
  const { data: decisions } = useListDecisions();

  // Local editable state for the latest draft version's Rego module +
  // the evaluate-pane input.
  const latestDraft = versions?.find((v) => v.status === 1); // draft
  const [rego, setRego] = useState("");
  const [query, setQuery] = useState("");
  const [testInput, setTestInput] = useState('{"target":{"type":"work_item","id":"task_01"}}');
  const [evalResult, setEvalResult] = useState<null | {
    effect: number;
    error: string;
    trace: string;
    result: string;
  }>(null);

  // Seed local state from the latest draft when it arrives.
  if (latestDraft && rego === "" && latestDraft.regoModule !== "") {
    setRego(latestDraft.regoModule);
    setQuery(latestDraft.query);
  }

  if (isLoading || !data) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }

  const { policy, latestVersion } = data;
  const isDraft = policy.status === 1; // draft

  const handleSave = async () => {
    if (!latestDraft) return;
    await updateVersion.mutateAsync({
      policyId: id,
      decisionPoint: latestDraft.decisionPoint,
      scope: latestDraft.scope,
      scopeRef: latestDraft.scopeRef,
      effect: latestDraft.effect,
      regoModule: rego,
      query,
      versionNote: latestDraft.versionNote,
    });
  };

  const handlePublish = async () => {
    if (rego) {
      await handleSave();
    }
    await publishPolicy.mutateAsync({ policyId: id });
  };

  const handleTest = async () => {
    const res = await evaluatePolicy.mutateAsync({
      policyId: id,
      policyVersion: latestDraft?.version ?? 0,
      decisionPoint: latestDraft?.decisionPoint ?? DecisionPoint.DISPATCH,
      input: testInput,
    });
    setEvalResult({
      effect: res.effect,
      error: res.error,
      trace: res.trace,
      result: res.result,
    });
  };

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex min-w-0 items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/policies" })}
            className="shrink-0"
          >
            <ArrowLeft className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <div className="min-w-0">
            <h1 className="text-lg font-semibold tracking-tight sm:text-2xl truncate">{policy.name}</h1>
            <p className="truncate text-sm text-muted-foreground">
              {DECISION_POINT_LABELS[latestVersion?.decisionPoint ?? latestDraft?.decisionPoint ?? 0] ?? "—"} ·{" "}
              <span className={cn("font-medium", STATUS_STYLES[policy.status])}>
                {STATUS_LABELS[policy.status]}
              </span>
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {policy.status === 2 && (
            <Button
              variant="outline"
              onClick={() => supersedePolicy.mutate(id)}
            >
              Supersede
            </Button>
          )}
          {isDraft && (
            <>
              <Button variant="outline" onClick={handleSave} disabled={updateVersion.isPending}>
                Save draft
              </Button>
              <Button onClick={handlePublish} disabled={publishPolicy.isPending}>
                {publishPolicy.isPending ? "Publishing…" : "Publish"}
              </Button>
            </>
          )}
        </div>
      </div>

      <div className="grid gap-6 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Rego module</CardTitle>
            <CardDescription>
              Edit the draft version's Rego source. Published versions are
              immutable (docs/02 §2.5).
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            {latestDraft ? (
              <>
                <div className="space-y-2">
                  <Label>Query</Label>
                  <Input
                    className="font-mono text-xs"
                    value={query}
                    onChange={(e) => setQuery(e.target.value)}
                    placeholder="data.orchicon.policy.allow"
                  />
                </div>
                <div className="space-y-2">
                  <Label>Rego module</Label>
                  <Textarea
                    rows={18}
                    className="font-mono text-xs"
                    value={rego}
                    onChange={(e) => setRego(e.target.value)}
                  />
                </div>
              </>
            ) : latestVersion ? (
              <Textarea
                rows={18}
                className="font-mono text-xs"
                readOnly
                value={latestVersion.regoModule}
              />
            ) : (
              <p className="text-sm text-muted-foreground">No versions.</p>
            )}
          </CardContent>
        </Card>

        <div className="space-y-6">
          <Card>
            <CardHeader>
              <CardTitle>Test (dry-run)</CardTitle>
              <CardDescription>
                Evaluate the draft against an input document without
                persisting a decision (EvaluatePolicy — docs/07 §3.5).
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-4">
              <div className="space-y-2">
                <Label>Input (JSON)</Label>
                <Textarea
                  rows={5}
                  className="font-mono text-xs"
                  value={testInput}
                  onChange={(e) => setTestInput(e.target.value)}
                />
              </div>
              <Button onClick={handleTest} disabled={evaluatePolicy.isPending}>
                {evaluatePolicy.isPending ? "Evaluating…" : "Evaluate"}
              </Button>
              {evalResult && (
                <div className="space-y-2 rounded-md border bg-muted/30 p-3 text-xs">
                  <div>
                    <span className="text-muted-foreground">Effect: </span>
                    <span className="font-medium">
                      {EFFECT_LABELS[evalResult.effect] ?? "unspecified"}
                    </span>
                  </div>
                  {evalResult.error && (
                    <p className="text-destructive">{evalResult.error}</p>
                  )}
                  <details>
                    <summary className="cursor-pointer text-muted-foreground">
                      Result
                    </summary>
                    <pre className="mt-1 overflow-auto font-mono">
                      {evalResult.result}
                    </pre>
                  </details>
                  <details>
                    <summary className="cursor-pointer text-muted-foreground">
                      Rego trace
                    </summary>
                    <pre className="mt-1 max-h-64 overflow-auto font-mono">
                      {evalResult.trace}
                    </pre>
                  </details>
                </div>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Decision log</CardTitle>
              <CardDescription>
                Recent recorded evaluations (ListDecisions — docs/07 §3.5).
                Drill-down to the Rego trace via ExplainDecision.
              </CardDescription>
            </CardHeader>
            <CardContent>
              {!decisions || decisions.length === 0 ? (
                <p className="text-sm text-muted-foreground">No decisions yet.</p>
              ) : (
                <ul className="space-y-2 text-xs">
                  {decisions.slice(0, 20).map((d) => (
                    <li
                      key={d.id}
                      className="flex items-center justify-between rounded-md border px-3 py-2"
                    >
                      <span className="font-mono truncate">
                        {d.decisionPoint || "—"}
                      </span>
                      <span
                        className={cn(
                          "rounded-full px-2 py-0.5 font-medium",
                          EFFECT_STYLES[d.effect],
                        )}
                      >
                        {EFFECT_LABELS[d.effect] ?? "unspecified"}
                      </span>
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Card>
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Versions</CardTitle>
        </CardHeader>
        <CardContent>
          <ul className="space-y-2 text-sm">
            {(versions ?? []).map((v) => (
              <li key={v.id} className="flex items-center justify-between">
                <span className="font-mono">v{v.version}</span>
                <span className="text-muted-foreground">{v.versionNote}</span>
                <span
                  className={cn(
                    "rounded-full px-2 py-0.5 text-xs",
                    VERSION_STATUS_STYLES[v.status],
                  )}
                >
                  {VERSION_STATUS_LABELS[v.status]}
                </span>
              </li>
            ))}
          </ul>
        </CardContent>
      </Card>
    </div>
  );
}

const DECISION_POINT_LABELS: Record<number, string> = {
  1: "admission",
  2: "dispatch",
  3: "budget",
  4: "approval",
  5: "recovery",
  6: "completion",
};

const STATUS_LABELS: Record<number, string> = {
  1: "draft",
  2: "published",
  3: "superseded",
};
const STATUS_STYLES: Record<number, string> = {
  1: "text-blue-600",
  2: "text-green-600",
  3: "text-yellow-600",
};

const EFFECT_LABELS: Record<number, string> = {
  1: "allow",
  2: "deny",
  3: "require_approval",
  4: "require_review",
};
const EFFECT_STYLES: Record<number, string> = {
  1: "bg-green-100 text-green-800",
  2: "bg-red-100 text-red-800",
  3: "bg-yellow-100 text-yellow-800",
  4: "bg-purple-100 text-purple-800",
};

const VERSION_STATUS_LABELS: Record<number, string> = {
  1: "draft",
  2: "published",
  3: "superseded",
};
const VERSION_STATUS_STYLES: Record<number, string> = {
  1: "bg-blue-100 text-blue-800",
  2: "bg-green-100 text-green-800",
  3: "bg-yellow-100 text-yellow-800",
};
