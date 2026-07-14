import { zodResolver } from "@hookform/resolvers/zod";
import { createRoute, useNavigate } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { useCreateWorker } from "@/api/workers";
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

// Create worker form (docs/10 §5, §2: React Hook Form + Zod).
//
// The Worker entity (docs/05_Worker_Specification.md §3) carries:
//   - Identity: name, slug, description, purpose
//   - Execution profile: runtime_ref, model_ref, system_prompt, context_sources
//   - Governance: permissions, gated_tools, budget_overrides, concurrency_limit
//
// Zod validation mirrors the server-side rules (internal/worker/validate.go)
// so the form rejects invalid input before round-tripping. JSON fields
// (permissions, gated_tools, budget_overrides, context_sources, labels)
// are validated as valid JSON before submission.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workers/new",
  component: NewWorkerPage,
});

const slugRegex = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

const createWorkerSchema = z.object({
  name: z
    .string()
    .min(1, "Name is required")
    .max(200, "Name must be at most 200 characters"),
  slug: z
    .string()
    .max(63, "Slug must be at most 63 characters")
    .regex(slugRegex, "Slug must be lowercase alphanumeric with hyphens")
    .optional()
    .or(z.literal("")),
  description: z.string().max(16000, "Description is too long").optional(),
  purpose: z.string().max(16000, "Purpose is too long").optional(),
  runtimeRef: z
    .string()
    .min(1, "Runtime ref is required")
    .max(200, "Runtime ref is too long"),
  modelRef: z
    .string()
    .min(1, "Model ref is required")
    .max(200, "Model ref is too long"),
  systemPrompt: z
    .string()
    .max(1_048_576, "System prompt is too large")
    .optional(),
  contextSources: z
    .string()
    .max(1_048_576, "Context sources is too large")
    .refine((v) => v === "" || v === undefined || isValidJson(v), {
      message: "Context sources must be valid JSON (array)",
    })
    .optional(),
  permissions: z
    .string()
    .max(1_048_576, "Permissions is too large")
    .refine((v) => v === "" || v === undefined || isValidJson(v), {
      message: "Permissions must be valid JSON",
    })
    .optional(),
  gatedTools: z
    .string()
    .max(1_048_576, "Gated tools is too large")
    .refine(
      (v) =>
        v === "" ||
        v === undefined ||
        isValidJson(v),
      { message: "Gated tools must be valid JSON (array)" },
    )
    .optional(),
  budgetOverrides: z
    .string()
    .max(1_048_576, "Budget overrides is too large")
    .refine((v) => v === "" || v === undefined || isValidJson(v), {
      message: "Budget overrides must be valid JSON",
    })
    .optional(),
  concurrencyLimit: z.number().int().min(0).max(1000),
  versionNote: z.string().max(16000, "Version note is too long").optional(),
});

type CreateWorkerForm = z.infer<typeof createWorkerSchema>;

const DEFAULT_PERMISSIONS = `{
  "tools": ["file_edit"],
  "mcp_servers": [],
  "model_providers": [],
  "context": [],
  "network": [],
  "filesystem": []
}`;

const DEFAULT_BUDGETS = `{
  "tokens": 1000000,
  "cost_usd": 10,
  "wall_clock_seconds": 3600,
  "tool_call_count": 100
}`;

function NewWorkerPage() {
  const navigate = useNavigate();
  const createWorker = useCreateWorker();
  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<CreateWorkerForm>({
    resolver: zodResolver(createWorkerSchema),
    defaultValues: {
      name: "",
      slug: "",
      description: "",
      purpose: "",
      runtimeRef: "opencode",
      modelRef: "",
      systemPrompt: "",
      contextSources: "[]",
      permissions: DEFAULT_PERMISSIONS,
      gatedTools: "[]",
      budgetOverrides: DEFAULT_BUDGETS,
      concurrencyLimit: 1,
      versionNote: "",
    },
  });

  const onSubmit = async (values: CreateWorkerForm) => {
    const result = await createWorker.mutateAsync({
      name: values.name,
      slug: values.slug || undefined,
      description: values.description || undefined,
      purpose: values.purpose || undefined,
      runtimeRef: values.runtimeRef,
      modelRef: values.modelRef,
      systemPrompt: values.systemPrompt || undefined,
      contextSources: values.contextSources || undefined,
      permissions: values.permissions || undefined,
      gatedTools: values.gatedTools || undefined,
      budgetOverrides: values.budgetOverrides || undefined,
      concurrencyLimit: values.concurrencyLimit,
      versionNote: values.versionNote || undefined,
    });
    navigate({ to: "/workers/$id", params: { id: result.worker.id } });
  };

  return (
    <div className="mx-auto max-w-3xl space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">New Worker</h1>
        <p className="text-sm text-muted-foreground">
          A Worker is a reusable execution profile. It starts in draft state;
          publish a version to make it dispatchable.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Identity</CardTitle>
          <CardDescription>
            The worker's name, slug, and purpose. The slug is unique within
            the tenant.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit(onSubmit)} className="space-y-6" noValidate>
            <div className="space-y-2">
              <Label htmlFor="name">Name</Label>
              <Input
                id="name"
                placeholder="Implementer"
                {...register("name")}
              />
              {errors.name && (
                <p className="text-xs text-destructive">
                  {errors.name.message}
                </p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="slug">Slug (optional)</Label>
              <Input
                id="slug"
                placeholder="implementer"
                {...register("slug")}
              />
              {errors.slug ? (
                <p className="text-xs text-destructive">
                  {errors.slug.message}
                </p>
              ) : (
                <p className="text-xs text-muted-foreground">
                  Derived from the name if left blank.
                </p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="purpose">Purpose</Label>
              <Input
                id="purpose"
                placeholder="Writes and refactors code"
                {...register("purpose")}
              />
              {errors.purpose && (
                <p className="text-xs text-destructive">
                  {errors.purpose.message}
                </p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="description">Description</Label>
              <Textarea
                id="description"
                placeholder="A general-purpose coding worker…"
                rows={3}
                {...register("description")}
              />
              {errors.description && (
                <p className="text-xs text-destructive">
                  {errors.description.message}
                </p>
              )}
            </div>

            <CardHeader className="px-0">
              <CardTitle>Execution profile</CardTitle>
              <CardDescription>
                The model, runtime, and system prompt. Template variables
                like {"{{project.name}}"} and {"{{task.title}}"} are resolved
                by the control plane before dispatch (docs/05 §11).
              </CardDescription>
            </CardHeader>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <Label htmlFor="runtimeRef">Runtime ref</Label>
                <Input
                  id="runtimeRef"
                  placeholder="opencode"
                  {...register("runtimeRef")}
                />
                {errors.runtimeRef && (
                  <p className="text-xs text-destructive">
                    {errors.runtimeRef.message}
                  </p>
                )}
              </div>
              <div className="space-y-2">
                <Label htmlFor="modelRef">Model ref</Label>
                <Input
                  id="modelRef"
                  placeholder="anthropic/claude-sonnet-4"
                  {...register("modelRef")}
                />
                {errors.modelRef && (
                  <p className="text-xs text-destructive">
                    {errors.modelRef.message}
                  </p>
                )}
              </div>
            </div>

            <div className="space-y-2">
              <Label htmlFor="systemPrompt">System prompt</Label>
              <Textarea
                id="systemPrompt"
                placeholder="You are an expert software engineer. Use {{project.name}} context…"
                rows={8}
                className="font-mono text-xs"
                {...register("systemPrompt")}
              />
              {errors.systemPrompt && (
                <p className="text-xs text-destructive">
                  {errors.systemPrompt.message}
                </p>
              )}
            </div>

            <CardHeader className="px-0">
              <CardTitle>Governance</CardTitle>
              <CardDescription>
                Permissions (allowlist), gated tools (per-call policy),
                budget overrides, and concurrency (docs/05 §7, §8, §9).
              </CardDescription>
            </CardHeader>

            <div className="space-y-2">
              <Label htmlFor="permissions">
                Permissions (JSON allowlist)
              </Label>
              <Textarea
                id="permissions"
                rows={6}
                className="font-mono text-xs"
                {...register("permissions")}
              />
              {errors.permissions && (
                <p className="text-xs text-destructive">
                  {errors.permissions.message}
                </p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="gatedTools">
                Gated tools (JSON array — Tier 2 per-call gating)
              </Label>
              <Input
                id="gatedTools"
                placeholder='["terminal", "git"]'
                className="font-mono text-xs"
                {...register("gatedTools")}
              />
              {errors.gatedTools && (
                <p className="text-xs text-destructive">
                  {errors.gatedTools.message}
                </p>
              )}
              <p className="text-xs text-muted-foreground">
                v0.1 supports gating: terminal, web_fetch, git. Empty = Tier
                1 only (dispatch-time).
              </p>
            </div>

            <div className="space-y-2">
              <Label htmlFor="budgetOverrides">
                Budget overrides (JSON)
              </Label>
              <Textarea
                id="budgetOverrides"
                rows={5}
                className="font-mono text-xs"
                {...register("budgetOverrides")}
              />
              {errors.budgetOverrides && (
                <p className="text-xs text-destructive">
                  {errors.budgetOverrides.message}
                </p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="contextSources">
                Context sources (JSON array)
              </Label>
              <Input
                id="contextSources"
                placeholder='["project_docs", "file_tree"]'
                className="font-mono text-xs"
                {...register("contextSources")}
              />
              {errors.contextSources && (
                <p className="text-xs text-destructive">
                  {errors.contextSources.message}
                </p>
              )}
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <Label htmlFor="concurrencyLimit">Concurrency limit</Label>
                <Input
                  id="concurrencyLimit"
                  type="number"
                  min={0}
                  max={1000}
                  {...register("concurrencyLimit", { valueAsNumber: true })}
                />
                {errors.concurrencyLimit && (
                  <p className="text-xs text-destructive">
                    {errors.concurrencyLimit.message}
                  </p>
                )}
              </div>
              <div className="space-y-2">
                <Label htmlFor="versionNote">Version note</Label>
                <Input
                  id="versionNote"
                  placeholder="Initial version"
                  {...register("versionNote")}
                />
                {errors.versionNote && (
                  <p className="text-xs text-destructive">
                    {errors.versionNote.message}
                  </p>
                )}
              </div>
            </div>

            {createWorker.error && (
              <p className="text-sm text-destructive">
                Failed to create worker: {String(createWorker.error)}
              </p>
            )}

            <div className="flex justify-end gap-2">
              <Button
                type="button"
                variant="outline"
                onClick={() => navigate({ to: "/workers" })}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={isSubmitting}>
                {isSubmitting ? "Creating…" : "Create Worker"}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}

function isValidJson(s: string): boolean {
  try {
    JSON.parse(s);
    return true;
  } catch {
    return false;
  }
}
