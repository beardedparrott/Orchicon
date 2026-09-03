import { zodResolver } from "@hookform/resolvers/zod";
import { createRoute, useNavigate } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { useCreateWorkflow } from "@/api/workflows";
import { useListProjects } from "@/api/projects";
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
import { GitStrategySelect, gitStrategyToProto } from "@/components/GitStrategySelect";
import { GitStrategy } from "@/api/gen/orchicon/api/v1/project_pb";
import { Route as rootRoute } from "@/routes/__root";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workflows/new",
  component: NewWorkflowPage,
});

const createWorkflowSchema = z.object({
  name: z
    .string()
    .min(1, "Name is required")
    .max(500, "Name must be at most 500 characters"),
  type: z.enum(["one-shot", "repeatable-template"]),
  projectId: z.string().optional(),
  versionNote: z.string().max(16384, "Version note is too long").optional(),
  gitStrategy: z.enum(["inherit", "local", "pr", "none"]).default("inherit"),
});

type CreateWorkflowForm = z.input<typeof createWorkflowSchema>;

function NewWorkflowPage() {
  const navigate = useNavigate();
  const createWorkflow = useCreateWorkflow();
  const { data: projects } = useListProjects();
  const {
    register,
    watch,
    setValue,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<CreateWorkflowForm>({
    resolver: zodResolver(createWorkflowSchema),
    defaultValues: { name: "", type: "one-shot", versionNote: "", gitStrategy: "inherit" },
  });
  const workflowType = watch("type");
  const gitStrategy = watch("gitStrategy");

  const onSubmit = async (values: CreateWorkflowForm) => {
    const rawPayload = values.gitStrategy === "inherit" ? undefined : values.gitStrategy;
    const gitStrategyPayload = rawPayload ? gitStrategyToProto(rawPayload) : undefined;
    const res = await createWorkflow.mutateAsync({
      name: values.name,
      projectId: values.type === "one-shot" ? (values.projectId ?? "") : "",
      type: values.type === "repeatable-template" ? "template" : "one_shot",
      steps: "[]",
      inputs: "{}",
      outputs: "{}",
      versionNote: values.versionNote ?? "",
      gitStrategy: gitStrategyPayload ?? GitStrategy.UNSPECIFIED,
    });
    navigate({ to: "/workflows/$id", params: { id: res.workflow.id } });
  };

  return (
    <div className="mx-auto max-w-2xl space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">New Workflow</h1>
        <p className="text-sm text-muted-foreground">
          Choose whether this is a one-shot project workflow or a repeatable
          template that can be bound to work items.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Workflow details</CardTitle>
          <CardDescription>
            A workflow starts in draft state. After creating, open the visual
            editor to add steps. Publish to make it runnable.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit(onSubmit)} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="name">Name</Label>
              <Input
                id="name"
                placeholder="Release cut pipeline"
                {...register("name")}
              />
              {errors.name && (
                <p className="text-xs text-destructive">
                  {errors.name.message}
                </p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="type">Type</Label>
              <select
                id="type"
                {...register("type")}
                className="flex h-11 sm:h-9 min-h-[44px] w-full rounded-xl glass-input px-3 py-1 text-sm"
              >
                <option value="one-shot">One-Shot</option>
                <option value="repeatable-template">Repeatable Template</option>
              </select>
              <p className="text-xs text-muted-foreground">
                {workflowType === "one-shot"
                  ? "A single-run workflow tied to a project. Use the canvas to define project, work items, workers, and steps. Run it once."
                  : "A reusable template bound to any work item. The template defines the workers; the work item provides the context. Can auto-start or run on a schedule."}
              </p>
            </div>

            {workflowType === "one-shot" && (
              <div className="space-y-2">
                <Label htmlFor="projectId">Project</Label>
                <select
                  id="projectId"
                  {...register("projectId")}
                  className="flex h-11 sm:h-9 min-h-[44px] w-full rounded-xl glass-input px-3 py-1 text-sm"
                >
                  <option value="">— Select a project —</option>
                  {(projects ?? []).map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </select>
                {errors.projectId && (
                  <p className="text-xs text-destructive">
                    {errors.projectId.message}
                  </p>
                )}
              </div>
            )}

            <GitStrategySelect
              value={gitStrategy ?? "inherit"}
              onValueChange={(v) => setValue("gitStrategy", v)}
              includeInherit
              inheritDescription={
                workflowType === "one-shot"
                  ? "Inherit from the selected project — uses the project's git strategy (local / PR / ephemeral). One-shot runs are still worktree-isolated. Recommended for most project workflows."
                  : "Inherit from the work item's project at run time — the worker will use whatever git strategy the bound project is configured to use. Works for both one-shot and scheduled template runs."
              }
            />
            <p className="text-xs text-muted-foreground">
              One-shot and template workflows both support this override. Canvas placement is still TBD — see discussion below.
            </p>

            <div className="space-y-2">
              <Label htmlFor="versionNote">Version note (optional)</Label>
              <Input
                id="versionNote"
                placeholder="Initial draft"
                {...register("versionNote")}
              />
              {errors.versionNote && (
                <p className="text-xs text-destructive">
                  {errors.versionNote.message}
                </p>
              )}
            </div>

            <div className="flex justify-end gap-2 pt-2">
              <Button
                type="button"
                variant="outline"
                onClick={() => navigate({ to: "/workflows" })}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={isSubmitting}>
                {isSubmitting ? "Creating…" : "Create workflow"}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      <Card className="border-dashed">
        <CardHeader>
          <CardTitle className="text-sm">Canvas placement — discussion</CardTitle>
          <CardDescription>
            Where should the workflow-level override live in the visual editor?
          </CardDescription>
        </CardHeader>
        <CardContent className="text-xs text-muted-foreground space-y-2">
          <p>Options under consideration:</p>
          <ul className="list-disc pl-4 space-y-1">
            <li><span className="font-medium">Workflow header bar</span> — next to the workflow name / type, as a persistent badge + dropdown (always visible).</li>
            <li><span className="font-medium">Settings pane</span> — in the canvas right-hand properties panel when no step is selected.</li>
            <li><span className="font-medium">Start node</span> — as a property of the implicit Start node, so the canvas graph shows the strategy at the entry point.</li>
          </ul>
          <p>All three keep worktrees always-on; the UI just controls branch materialization. Feedback welcome — header bar is the current favorite for discoverability.</p>
        </CardContent>
      </Card>
    </div>
  );
}
