// New Recurring Item form (feature 4.3) — schedule-first, NOT a work-item
// clone. Kind/parent/priority are dropped (recurring rows are the flat
// shape: kind=task, no parent, status RECURRING). Added: workflow binding
// (required to schedule), runtime image, context files, cadence
// (RecurringScheduleForm), and an OPT-IN "outputs: ideas" toggle that sets
// the schedule's `outputs_mode` so a fire's spawned items land in IDEA state.

import { zodResolver } from "@hookform/resolvers/zod";
import { createRoute, useNavigate, useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { useCreateWorkItem } from "@/api/workItems";
import { useListProjects } from "@/api/projects";
import { useListWorkflows } from "@/api/workflows";
import { FileBrowser } from "@/components/FileBrowser";
import { SecretsPicker } from "@/components/SecretsPicker";
import { RuntimeImageSelect } from "@/components/RuntimeImageSelect";
import { RecurringScheduleForm } from "@/components/work-items/RecurringScheduleForm";
import { RecurringSchedule } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { WorkItemKind } from "@/api/gen/orchicon/api/v1/work_item_pb";
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

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/recurring-items/new",
  component: NewRecurringItemPage,
  validateSearch: (search: Record<string, unknown>) => ({
    projectId: (search.projectId as string) ?? "",
  }),
});

const createRecurringSchema = z.object({
  title: z
    .string()
    .min(1, "Title is required")
    .max(500, "Title must be at most 500 characters"),
  // The cadence needs a runnable workflow — binding is required to schedule.
  workflowId: z
    .string()
    .min(1, "A workflow binding is required to schedule a recurring item"),
  description: z.string().max(1_048_576, "Description is too large").optional(),
  acceptanceCriteria: z
    .string()
    .max(1_048_576, "Acceptance criteria is too large")
    .optional(),
});

type CreateRecurringForm = z.infer<typeof createRecurringSchema>;

function NewRecurringItemPage() {
  const navigate = useNavigate();
  const search = useSearch({ from: "/recurring-items_/new" });
  const [selectedProjectId, setSelectedProjectId] = useState(search.projectId || "");
  const createWorkItem = useCreateWorkItem();
  const { data: projects } = useListProjects();
  const { data: workflows } = useListWorkflows({ status: 2, templatesOnly: true }); // published templates only

  const [runtimeImage, setRuntimeImage] = useState("");
  const [secretIds, setSecretIds] = useState<string[]>([]);
  const [contextFiles, setContextFiles] = useState<string[]>([]);
  const [recurringSchedule, setRecurringSchedule] = useState<RecurringSchedule | undefined>(undefined);
  // OPT-IN "outputs: ideas" provenance — sets the schedule's outputs_mode so
  // spawned items land in IDEA state (hidden from normal work-item views).
  const [ideaOutputs, setIdeaOutputs] = useState(false);
  const [scheduleError, setScheduleError] = useState("");

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<CreateRecurringForm>({
    resolver: zodResolver(createRecurringSchema),
    defaultValues: {
      title: "",
      workflowId: "",
      description: "",
      acceptanceCriteria: "",
    },
  });

  const selectedProject = projects?.find((p) => p.id === selectedProjectId);
  const hasSchedule = !!recurringSchedule?.frequency || !!recurringSchedule?.startDate || !!recurringSchedule?.startTime;

  const onSubmit = async (values: CreateRecurringForm) => {
    if (!hasSchedule) {
      setScheduleError("A recurring schedule is required — toggle it on and set a cadence.");
      return;
    }
    setScheduleError("");
    // Apply the outputs:ideas opt-in to the schedule's outputs_mode
    // ("standard" is the default; "idea" routes spawned items to IDEA state).
    const schedule = recurringSchedule
      ? new RecurringSchedule({
          ...recurringSchedule,
          outputsMode: ideaOutputs ? "idea" : "standard",
        })
      : undefined;
    const workItem = await createWorkItem.mutateAsync({
      projectId: selectedProjectId,
      // Flat recurring shape (migration D1): kind=task, no parent.
      kind: WorkItemKind.TASK,
      title: values.title,
      description: values.description || undefined,
      acceptanceCriteria: values.acceptanceCriteria || undefined,
      workflowId: values.workflowId,
      runtimeImage: runtimeImage || undefined,
      secretIds: secretIds.length > 0 ? secretIds : undefined,
      contextFiles: contextFiles.length > 0 ? contextFiles : undefined,
      recurringSchedule: schedule,
    });
    navigate({
      to: "/recurring-items/$id",
      params: { id: workItem.id },
    });
  };

  return (
    <div className="mx-auto max-w-2xl space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">New Recurring Item</h1>
        <p className="text-sm text-muted-foreground">
          A recurring item is a first-class automation: it fires its bound
          workflow on a cadence and records a per-fire history. No kind, no
          parent, no priority — just a schedule and a workflow.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Automation details</CardTitle>
          <CardDescription>
            Binding a workflow is required — the cadence needs something to run.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit(onSubmit)} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="project">Project</Label>
              <select
                id="project"
                className="flex h-11 min-h-[44px] w-full rounded-xl glass-input px-3 py-1 text-sm sm:h-9"
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
                placeholder="Weekly dependency audit"
                {...register("title")}
              />
              {errors.title && (
                <p className="text-xs text-destructive">{errors.title.message}</p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="workflow">Workflow</Label>
              <select
                id="workflow"
                className="flex h-11 min-h-[44px] w-full rounded-xl glass-input px-3 py-1 text-sm sm:h-9"
                {...register("workflowId")}
              >
                <option value="">— Select a workflow —</option>
                {(workflows ?? []).map((wf) => (
                  <option key={wf.id} value={wf.id}>
                    {wf.name}
                  </option>
                ))}
              </select>
              {errors.workflowId && (
                <p className="text-xs text-destructive">{errors.workflowId.message}</p>
              )}
              {(workflows ?? []).length === 0 && (
                <p className="text-xs text-muted-foreground">
                  No published workflow templates yet — publish one in
                  Workflows to bind it here.
                </p>
              )}
            </div>

            <SecretsPicker value={secretIds} onChange={setSecretIds} />

            <div className="space-y-2">
              <Label htmlFor="runtimeImage">Runtime image</Label>
              <RuntimeImageSelect value={runtimeImage} onChange={setRuntimeImage} />
              <p className="text-xs text-muted-foreground">
                The container image workers run in for this automation&apos;s
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
                files or directories for this automation.
              </p>
            )}

            <div className="rounded-2xl glass-panel p-3 space-y-3">
              <RecurringScheduleForm
                value={recurringSchedule}
                onChange={(s) => {
                  setRecurringSchedule(s);
                  if (s && (!s.frequency || !s.startDate)) setScheduleError("");
                }}
              />
              <div className="flex items-start gap-2">
                <input
                  type="checkbox"
                  id="ideaOutputs"
                  checked={ideaOutputs}
                  disabled={!hasSchedule}
                  onChange={(e) => setIdeaOutputs(e.target.checked)}
                  className="mt-1 h-4 w-4 rounded border-input disabled:cursor-not-allowed disabled:opacity-50"
                />
                <div>
                  <Label htmlFor="ideaOutputs">Outputs: ideas</Label>
                  <p className="text-xs text-muted-foreground">
                    {hasSchedule
                      ? "Each fire's spawned work items land in IDEA state — hidden from normal Work Items until promoted."
                      : "Enable a recurring schedule to opt into idea-state outputs."}
                  </p>
                </div>
              </div>
              {scheduleError && (
                <p className="text-xs text-destructive">{scheduleError}</p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="description">Description (optional)</Label>
              <Textarea id="description" rows={4} {...register("description")} />
              {errors.description && (
                <p className="text-xs text-destructive">{errors.description.message}</p>
              )}
            </div>

            <div className="space-y-2">
              <Label htmlFor="acceptanceCriteria">Acceptance criteria (optional)</Label>
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
                Failed to create recurring item: {String(createWorkItem.error)}
              </p>
            )}

            {!selectedProjectId && (
              <p
                className="text-xs text-destructive"
                role="alert"
                aria-label="Project required"
              >
                Select a project before saving.
              </p>
            )}
            {hasSchedule && ideaOutputs && (
              <p className="text-xs text-muted-foreground">
                Each fire&apos;s spawned work items will land in IDEA state —
                hidden from normal Work Items until promoted.
              </p>
            )}

            <div className="flex justify-end gap-2">
              <Button
                type="button"
                variant="outline"
                onClick={() => navigate({ to: "/recurring-items" })}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={isSubmitting || !selectedProjectId || !hasSchedule}>
                {isSubmitting ? "Creating…" : "Create Recurring Item"}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
