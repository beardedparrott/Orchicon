import { zodResolver } from "@hookform/resolvers/zod";
import { createRoute, useNavigate } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { useCreatePolicy } from "@/api/policies";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Route as rootRoute } from "@/routes/__root";
import {
  DecisionPoint,
  PolicyEffect,
  PolicyScope,
} from "@/api/gen/orchicon/api/v1/policy_pb";

// Create policy form (docs/02 §2.5). The Rego module is the policy
// source; the default query is `data.orchicon.policy.allow`.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/policies/new",
  component: NewPolicyPage,
});

const DEFAULT_REGO = `package orchicon.policy

import rego.v1

# Default-allow policy. Return true to assert the policy's effect;
# return false (or omit) to fall through to the next candidate.
# Narrowest scope wins; first definitive decision wins (docs/02 §2.5).
default allow := false

allow if {
    input.target.type == "work_item"
}

# Example: require approval for terminal tool access
# allow if {
#     input.worker.gated_tools[_] == "terminal"
#     input.effect == "require_approval"
# }
`;

const createPolicySchema = z.object({
  name: z.string().min(1, "Name is required").max(200),
  decisionPoint: z.number().int().min(1).max(6),
  scope: z.number().int().min(1).max(4),
  scopeRef: z.string().max(200).optional(),
  effect: z.number().int().min(1).max(4),
  regoModule: z.string().min(1, "Rego module is required"),
  query: z.string().max(1000).optional(),
  versionNote: z.string().max(16000).optional(),
});

type CreatePolicyForm = z.infer<typeof createPolicySchema>;

function NewPolicyPage() {
  const navigate = useNavigate();
  const createPolicy = useCreatePolicy();
  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<CreatePolicyForm>({
    resolver: zodResolver(createPolicySchema),
    defaultValues: {
      name: "",
      decisionPoint: DecisionPoint.DISPATCH,
      scope: PolicyScope.TENANT,
      scopeRef: "",
      effect: PolicyEffect.ALLOW,
      regoModule: DEFAULT_REGO,
      query: "",
      versionNote: "Initial version",
    },
  });

  const onSubmit = async (values: CreatePolicyForm) => {
    const result = await createPolicy.mutateAsync({
      name: values.name,
      decisionPoint: values.decisionPoint,
      scope: values.scope,
      scopeRef: values.scopeRef ?? "",
      effect: values.effect,
      regoModule: values.regoModule,
      query: values.query ?? "",
      versionNote: values.versionNote ?? "",
    });
    navigate({ to: "/policies/$id", params: { id: result.policy.id } });
  };

  return (
    <div className="mx-auto max-w-3xl space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">New Policy</h1>
        <p className="text-sm text-muted-foreground">
          A Rego module evaluated at a decision point. Starts in draft;
          publish to compile + activate it.
        </p>
      </div>

      <Card>
        <CardContent>
          <form onSubmit={handleSubmit(onSubmit)} className="space-y-6">
            <div className="space-y-2">
              <Label htmlFor="name">Name</Label>
              <Input id="name" placeholder="Dispatch gate" {...register("name")} />
              {errors.name && (
                <p className="text-xs text-destructive">{errors.name.message}</p>
              )}
            </div>

            <div className="grid gap-4 md:grid-cols-3">
              <div className="space-y-2">
                <Label htmlFor="decisionPoint">Decision point</Label>
                <select
                  id="decisionPoint"
                  className="w-full rounded-md border bg-background px-3 py-2 text-sm"
                  {...register("decisionPoint", { valueAsNumber: true })}
                >
                  <option value={DecisionPoint.ADMISSION}>Admission</option>
                  <option value={DecisionPoint.DISPATCH}>Dispatch</option>
                  <option value={DecisionPoint.BUDGET}>Budget</option>
                  <option value={DecisionPoint.APPROVAL}>Approval</option>
                  <option value={DecisionPoint.RECOVERY}>Recovery</option>
                  <option value={DecisionPoint.COMPLETION}>Completion</option>
                </select>
              </div>
              <div className="space-y-2">
                <Label htmlFor="scope">Scope</Label>
                <select
                  id="scope"
                  className="w-full rounded-md border bg-background px-3 py-2 text-sm"
                  {...register("scope", { valueAsNumber: true })}
                >
                  <option value={PolicyScope.TENANT}>Tenant</option>
                  <option value={PolicyScope.PROJECT}>Project</option>
                  <option value={PolicyScope.WORKER}>Worker</option>
                  <option value={PolicyScope.TASK}>Task</option>
                </select>
              </div>
              <div className="space-y-2">
                <Label htmlFor="effect">Effect</Label>
                <select
                  id="effect"
                  className="w-full rounded-md border bg-background px-3 py-2 text-sm"
                  {...register("effect", { valueAsNumber: true })}
                >
                  <option value={PolicyEffect.ALLOW}>Allow</option>
                  <option value={PolicyEffect.DENY}>Deny</option>
                  <option value={PolicyEffect.REQUIRE_APPROVAL}>Require approval</option>
                  <option value={PolicyEffect.REQUIRE_REVIEW}>Require review</option>
                </select>
              </div>
            </div>

            <div className="space-y-2">
              <Label htmlFor="scopeRef">Scope ref (project_id / worker_id — empty for tenant)</Label>
              <Input id="scopeRef" placeholder="" {...register("scopeRef")} />
            </div>

            <div className="space-y-2">
              <Label htmlFor="regoModule">Rego module</Label>
              <Textarea
                id="regoModule"
                rows={14}
                className="font-mono text-xs"
                {...register("regoModule")}
              />
              {errors.regoModule && (
                <p className="text-xs text-destructive">{errors.regoModule.message}</p>
              )}
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <Label htmlFor="query">Query (optional — default: data.orchicon.policy.allow)</Label>
                <Input
                  id="query"
                  placeholder="data.orchicon.policy.allow"
                  className="font-mono text-xs"
                  {...register("query")}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="versionNote">Version note</Label>
                <Input id="versionNote" {...register("versionNote")} />
              </div>
            </div>

            {createPolicy.error && (
              <p className="text-sm text-destructive">
                Failed to create policy: {String(createPolicy.error)}
              </p>
            )}

            <div className="flex justify-end gap-2">
              <Button
                type="button"
                variant="outline"
                onClick={() => navigate({ to: "/policies" })}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={isSubmitting}>
                {isSubmitting ? "Creating…" : "Create Policy"}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
