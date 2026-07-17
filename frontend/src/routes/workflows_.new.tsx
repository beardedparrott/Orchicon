import { zodResolver } from "@hookform/resolvers/zod";
import { createRoute, useNavigate } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { useCreateWorkflow } from "@/api/workflows";
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
    .max(500, "Name must be at most 500 characters"),
  versionNote: z.string().max(16384, "Version note is too long").optional(),
  recoveryPolicyRef: z.string().max(200).optional(),
});

type CreateWorkflowForm = z.infer<typeof createWorkflowSchema>;

function NewWorkflowPage() {
  const navigate = useNavigate();
  const createWorkflow = useCreateWorkflow();
  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<CreateWorkflowForm>({
    resolver: zodResolver(createWorkflowSchema),
    defaultValues: { name: "", versionNote: "", recoveryPolicyRef: "" },
  });

  const onSubmit = async (values: CreateWorkflowForm) => {
    const res = await createWorkflow.mutateAsync({
      name: values.name,
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
          A composable execution plan. Starts as a draft. Open the visual
          editor to drag connectors and wire steps. Use a Project connector
          to bind the workflow to a project.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Workflow details</CardTitle>
          <CardDescription>
            Workflows start in draft state. Drag a Project connector onto
            the canvas to bind it to a project, then publish to make it
            runnable.
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
