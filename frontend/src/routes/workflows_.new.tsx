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

// Create workflow form (docs/10 §5, §2: React Hook Form + Zod).
//
// A workflow starts in draft state with its first draft version (an
// empty step DAG). The visual editor opens after creation to add steps.
// project_id is optional (empty for a tenant-level template).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workflows/new",
  component: NewWorkflowPage,
});

const createWorkflowSchema = z.object({
  name: z
    .string()
    .min(1, "Name is required")
    .max(200, "Name must be at most 200 characters"),
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
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<CreateWorkflowForm>({
    resolver: zodResolver(createWorkflowSchema),
    defaultValues: { name: "", projectId: "", versionNote: "", recoveryPolicyRef: "" },
  });

  const onSubmit = async (values: CreateWorkflowForm) => {
    const res = await createWorkflow.mutateAsync({
      name: values.name,
      projectId: values.projectId ?? "",
      steps: "[]", // empty DAG — the editor adds steps
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
          A composable execution plan. Starts as a draft with an empty step
          DAG — open the visual editor to drag Workers and wire steps.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Workflow details</CardTitle>
          <CardDescription>
            The workflow is created in draft state. Publish it from the
            editor to make it runnable.
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
              <Label htmlFor="projectId">Project (optional)</Label>
              <select
                id="projectId"
                className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                {...register("projectId")}
              >
                <option value="">— tenant template —</option>
                {(projects ?? []).map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
              <p className="text-xs text-muted-foreground">
                Leave empty for a tenant-level reusable template. A
                project-scoped workflow runs against that project's work.
              </p>
            </div>

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
