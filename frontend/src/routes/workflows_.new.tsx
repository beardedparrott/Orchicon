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
  recoveryPolicyRef: z.string().max(200).optional(),
});

type CreateWorkflowForm = z.infer<typeof createWorkflowSchema>;

function NewWorkflowPage() {
  const navigate = useNavigate();
  const createWorkflow = useCreateWorkflow();
  const { data: projects } = useListProjects();
  const {
    register,
    watch,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<CreateWorkflowForm>({
    resolver: zodResolver(createWorkflowSchema),
    defaultValues: { name: "", type: "one-shot", versionNote: "", recoveryPolicyRef: "" },
  });
  const workflowType = watch("type");

  const onSubmit = async (values: CreateWorkflowForm) => {
    const res = await createWorkflow.mutateAsync({
      name: values.name,
      projectId: values.type === "one-shot" ? (values.projectId ?? "") : "",
      type: values.type === "repeatable-template" ? "template" : "one_shot",
      steps: "[]",
      inputs: "{}",
      outputs: "{}",
      recoveryPolicyRef: values.recoveryPolicyRef ?? "",
      versionNote: values.versionNote ?? "",
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
                className="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm"
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
                  className="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm"
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

            <div className="space-y-2">
              <Label htmlFor="recoveryPolicyRef">
                Recovery policy ref (optional)
              </Label>
              <Input
                id="recoveryPolicyRef"
                placeholder=""
                {...register("recoveryPolicyRef")}
              />
              {errors.recoveryPolicyRef && (
                <p className="text-xs text-destructive">
                  {errors.recoveryPolicyRef.message}
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
    </div>
  );
}
