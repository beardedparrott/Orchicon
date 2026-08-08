import { zodResolver } from "@hookform/resolvers/zod";
import { createRoute, useNavigate, useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { useCreateWorkItem } from "@/api/workItems";
import { useListWorkItems } from "@/api/workItems";
import { useListProjects } from "@/api/projects";
import { FileBrowser } from "@/components/FileBrowser";
import { RuntimeImageSelect } from "@/components/RuntimeImageSelect";
import { WorkItemParentSelect, depthForKind } from "@/components/work-items/work-item-parent-select";
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
  const [selectedProjectId, setSelectedProjectId] = useState(search.projectId || "");
  const parentId = search.parentId || "";
  const createWorkItem = useCreateWorkItem();
  const { data: projects } = useListProjects();

  // Fetch sibling items for parent selection.
  const { data: projectItems } = useListWorkItems(selectedProjectId);

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
    setValue,
    getValues,
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
  const [runtimeImage, setRuntimeImage] = useState("");
  const [contextFiles, setContextFiles] = useState<string[]>([]);
  const selectedProject = projects?.find((p) => p.id === selectedProjectId);

  // Changing the kind can invalidate the previously chosen parent: epics
  // have no parent, and a shallower kind cannot sit under a deeper one.
  // Clear a stale parent_id so the form never submits one the server
  // rejects with a generic InvalidArgument (and the picker shows its
  // "requires a parent" error instead of a silent placeholder).
  const clearStaleParent = (nextKind: CreateWorkItemForm["kind"]) => {
    const pid = getValues("parentId");
    if (!pid) return;
    if (nextKind === "epic") {
      setValue("parentId", "");
      return;
    }
    const parent = projectItems?.find((i) => i.id === pid);
    if (parent && depthForKind(parent.kind) >= KIND_TO_PROTO[nextKind]) {
      setValue("parentId", "");
    }
  };
  const kindRegister = register("kind");

  // The parent picker filters candidates by depth itself (only items
  // strictly shallower than the selected kind) — this mirrors the
  // server-side rule and is UX only (invariant #1).
  const onSubmit = async (values: CreateWorkItemForm) => {
    const workItem = await createWorkItem.mutateAsync({
      projectId: selectedProjectId,
      parentId: values.parentId || undefined,
      kind: KIND_TO_PROTO[values.kind],
      title: values.title,
      description: values.description || undefined,
      acceptanceCriteria: values.acceptanceCriteria || undefined,
      priority: values.priority,
      runtimeImage: runtimeImage || undefined,
      contextFiles: contextFiles.length > 0 ? contextFiles : undefined,
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
              <Label htmlFor="project">Project</Label>
              <select
                id="project"
                className="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm"
                value={selectedProjectId}
                onChange={(e) => setSelectedProjectId(e.target.value)}
              >
                <option value="">— Select project —</option>
                {(projects ?? []).map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
              {!selectedProjectId && (
                <p className="text-xs text-destructive">A project is required.</p>
              )}
            </div>

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
                  {...kindRegister}
                  onChange={(e) => {
                    void kindRegister.onChange(e);
                    clearStaleParent(e.target.value as CreateWorkItemForm["kind"]);
                  }}
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
                <WorkItemParentSelect
                  items={projectItems ?? []}
                  childKind={KIND_TO_PROTO[selectedKind as keyof typeof KIND_TO_PROTO]}
                  value={selectedParentId ?? ""}
                  onChange={(id) => setValue("parentId", id)}
                  invalid={!selectedParentId}
                  error={
                    !selectedParentId
                      ? `A ${selectedKind} requires a parent.`
                      : undefined
                  }
                />
              </div>
            )}

            <div className="space-y-2">
              <Label htmlFor="runtimeImage">Runtime image</Label>
              <RuntimeImageSelect value={runtimeImage} onChange={setRuntimeImage} />
              <p className="text-xs text-muted-foreground">
                The container image workers run in for this item&apos;s
                workflow. Defaults to the base image.
              </p>
            </div>

            {selectedProject?.projectDir ? (
              <FileBrowser
                projectId={selectedProject.id}
                projectDir={selectedProject.projectDir}
                initialSelectedFiles={contextFiles}
                onChange={setContextFiles}
                title="Work Item Context Files"
                description="Expand folders and check files or directories to include as context for the worker, exactly like project context files."
              />
            ) : (
              <p className="text-xs text-muted-foreground">
                Select a project with a project directory to add context
                files or directories for this work item.
              </p>
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
              <Button type="submit" disabled={isSubmitting || !selectedProjectId}>
                {isSubmitting ? "Creating…" : "Create Work Item"}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
