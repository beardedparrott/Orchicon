import { zodResolver } from "@hookform/resolvers/zod";
import { createRoute, useNavigate, useSearch } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { useCreateWorkItem } from "@/api/workItems";
import { useListWorkItems } from "@/api/workItems";
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

// Create work item form (docs/10 §5, §2). The kind determines the
// allowed parent (epic=none, otherwise any shallower kind).
// Zod validation mirrors the server-side rules
// (internal/workitem/validate.go).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work-items/new",
  component: NewWorkItemPage,
  validateSearch: (search: Record<string, unknown>) => ({
    projectId: (search.projectId as string) ?? "",
    parentId: (search.parentId as string) ?? "",
  }),
});

const createWorkItemSchema = z.object({
  title: z
    .string()
    .min(1, "Title is required")
    .max(500, "Title must be at most 500 characters"),
  kind: z.enum(["epic", "feature", "task", "subtask"], {
    message: "Kind must be one of: epic, feature, task, subtask",
  }),
  description: z.string().max(1_048_576, "Description is too large").optional(),
  acceptanceCriteria: z
    .string()
    .max(1_048_576, "Acceptance criteria is too large")
    .optional(),
  priority: z.number().int().min(0).max(1000),
  parentId: z.string().optional().or(z.literal("")),
});

type CreateWorkItemForm = z.infer<typeof createWorkItemSchema>;

const KIND_TO_PROTO: Record<string, number> = {
  epic: 1,
  feature: 2,
  task: 3,
  subtask: 4,
};

function NewWorkItemPage() {
  const navigate = useNavigate();
  const search = useSearch({ from: "/work-items_/new" });
  const projectId = search.projectId;
  const parentId = search.parentId || "";
  const createWorkItem = useCreateWorkItem();

  // Fetch sibling items to show context for parent selection.
  const { data: projectItems } = useListWorkItems(projectId);

  // Determine the default kind based on the parent (if any).
  const parentItem = projectItems?.find((i) => i.id === parentId);
  const defaultKind = parentItem
    ? parentItem.kind === 1
      ? "feature"
      : parentItem.kind === 2
        ? "task"
        : "subtask"
    : "epic";

  const {
    register,
    handleSubmit,
    watch,
    formState: { errors, isSubmitting },
  } = useForm<CreateWorkItemForm>({
    resolver: zodResolver(createWorkItemSchema),
    defaultValues: {
      title: "",
      kind: defaultKind as CreateWorkItemForm["kind"],
      description: "",
      acceptanceCriteria: "",
      priority: 0,
      parentId: parentId,
    },
  });

  const selectedKind = watch("kind");
  const selectedParentId = watch("parentId");

  // Validate parent/kind consistency — allow skipping levels (e.g.
  // Task under Epic), but reject same-level or deeper parents.
  const allowedParents = projectItems?.filter((i) => {
    if (selectedKind === "epic") return false; // epics have no parent
    const childDepth = depthForKind(KIND_TO_PROTO[selectedKind as keyof typeof KIND_TO_PROTO]);
    const parentDepth = depthForKind(i.kind);
    return parentDepth < childDepth;
  });

  const onSubmit = async (values: CreateWorkItemForm) => {
    const workItem = await createWorkItem.mutateAsync({
      projectId,
      parentId: values.parentId || undefined,
      kind: KIND_TO_PROTO[values.kind],
      title: values.title,
      description: values.description || undefined,
      acceptanceCriteria: values.acceptanceCriteria || undefined,
      priority: values.priority,
    });
    navigate({
      to: "/work-items/$id",
      params: { id: workItem.id },
    });
  };

  return (
    <div className="mx-auto max-w-2xl space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">New Work Item</h1>
        <p className="text-sm text-muted-foreground">
          Create an item in the work hierarchy. Epics are top-level; features,
          tasks, and subtasks nest under any shallower kind.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Work item details</CardTitle>
          <CardDescription>
            A new item starts in the pending state. Only tasks and subtasks
            are schedulable.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit(onSubmit)} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="title">Title</Label>
              <Input
                id="title"
                placeholder="Implement authentication"
                {...register("title")}
              />
              {errors.title && (
                <p className="text-xs text-destructive">
                  {errors.title.message}
                </p>
              )}
            </div>

            <div className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <Label htmlFor="kind">Kind</Label>
                <select
                  id="kind"
                  className="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm"
                  {...register("kind")}
                >
                  <option value="epic">Epic (top-level)</option>
                  <option value="feature">Feature</option>
                  <option value="task">Task</option>
                  <option value="subtask">Subtask</option>
                </select>
                {errors.kind && (
                  <p className="text-xs text-destructive">
                    {errors.kind.message}
                  </p>
                )}
              </div>

              <div className="space-y-2">
                <Label htmlFor="priority">Priority</Label>
                <Input
                  id="priority"
                  type="number"
                  min={0}
                  max={1000}
                  {...register("priority", { valueAsNumber: true })}
                />
                {errors.priority && (
                  <p className="text-xs text-destructive">
                    {errors.priority.message}
                  </p>
                )}
              </div>
            </div>

            {selectedKind !== "epic" && (
              <div className="space-y-2">
                <Label htmlFor="parentId">Parent</Label>
                <select
                  id="parentId"
                  className="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm"
                  {...register("parentId")}
                >
                  <option value="">— Select parent —</option>
                  {(allowedParents ?? []).map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.title}
                    </option>
                  ))}
                </select>
                {!selectedParentId && (
                  <p className="text-xs text-destructive">
                    A {selectedKind} requires a parent.
                  </p>
                )}
              </div>
            )}

            <div className="space-y-2">
              <Label htmlFor="description">Description (optional)</Label>
              <Textarea
                id="description"
                rows={4}
                {...register("description")}
              />
              {errors.description && (
                <p className="text-xs text-destructive">
                  {errors.description.message}
                </p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="acceptanceCriteria">
                Acceptance criteria (optional)
              </Label>
              <Textarea
                id="acceptanceCriteria"
                rows={3}
                {...register("acceptanceCriteria")}
              />
              {errors.acceptanceCriteria && (
                <p className="text-xs text-destructive">
                  {errors.acceptanceCriteria.message}
                </p>
              )}
            </div>

            {createWorkItem.error && (
              <p className="text-sm text-destructive">
                Failed to create work item: {String(createWorkItem.error)}
              </p>
            )}

            <div className="flex justify-end gap-2">
              <Button
                type="button"
                variant="outline"
                onClick={() => navigate({ to: "/work-items" })}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={isSubmitting}>
                {isSubmitting ? "Creating…" : "Create Work Item"}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}

function depthForKind(kind: number): number {
  return kind >= 1 && kind <= 4 ? kind : 0;
}
