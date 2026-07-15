import { zodResolver } from "@hookform/resolvers/zod";
import { createRoute, useNavigate } from "@tanstack/react-router";
import { useFieldArray, useForm } from "react-hook-form";
import { z } from "zod";

import { useCreateProject } from "@/api/projects";
import { GoalField } from "@/api/gen/orchicon/api/v1/project_pb";
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
  path: "/projects/new",
  component: NewProjectPage,
});

const slugRegex = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

const goalFieldSchema = z.object({
  key: z
    .string()
    .min(1, "Goal key is required")
    .max(100, "Goal key must be at most 100 characters"),
  value: z.string().max(10000, "Goal value is too long"),
});

const createProjectSchema = z.object({
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
  goals: z.array(goalFieldSchema).default([]),
});

type CreateProjectForm = z.input<typeof createProjectSchema>;

function NewProjectPage() {
  const navigate = useNavigate();
  const createProject = useCreateProject();
  const {
    register,
    control,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(createProjectSchema),
    defaultValues: { name: "", slug: "", goals: [{ key: "", value: "" }] },
  });

  const { fields, append, remove } = useFieldArray({
    control,
    name: "goals",
  });

  const onSubmit = async (values: CreateProjectForm) => {
    const goals = (values.goals ?? [])
      .filter((g) => g.key.trim() !== "")
      .map((g) => new GoalField(g));
    const project = await createProject.mutateAsync({
      name: values.name,
      slug: values.slug || undefined,
      goals: goals.length > 0 ? goals : undefined,
    });
    navigate({ to: "/projects/$id", params: { id: project.id } });
  };

  return (
    <div className="mx-auto max-w-2xl space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">New Project</h1>
        <p className="text-sm text-muted-foreground">
          Create a project to hold its work hierarchy, workflows, and
          execution history.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Project details</CardTitle>
          <CardDescription>
            A project starts in the drafting state; no execution is
            permitted until it is activated.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleSubmit(onSubmit)} className="space-y-4" noValidate>
            <div className="space-y-2">
              <Label htmlFor="name">Name</Label>
              <Input
                id="name"
                placeholder="My AI Project"
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
                placeholder="my-ai-project"
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
              <Label>Goals</Label>
              <p className="text-xs text-muted-foreground">
                Key-value pairs describing the project's objectives.
              </p>
              <div className="space-y-2">
                {fields.map((field, index) => (
                  <div key={field.id} className="flex items-start gap-2">
                    <div className="flex-1 space-y-1">
                      <Input
                        placeholder="Key (e.g. objective)"
                        {...register(`goals.${index}.key`)}
                      />
                      {errors.goals?.[index]?.key && (
                        <p className="text-xs text-destructive">
                          {errors.goals[index]!.key!.message}
                        </p>
                      )}
                    </div>
                    <div className="flex-1 space-y-1">
                      <Input
                        placeholder="Value (e.g. Build and ship the MVP)"
                        {...register(`goals.${index}.value`)}
                      />
                      {errors.goals?.[index]?.value && (
                        <p className="text-xs text-destructive">
                          {errors.goals[index]!.value!.message}
                        </p>
                      )}
                    </div>
                    {fields.length > 1 && (
                      <Button
                        type="button"
                        variant="ghost"
                        size="icon"
                        className="mt-0 shrink-0"
                        onClick={() => remove(index)}
                      >
                        ×
                      </Button>
                    )}
                  </div>
                ))}
              </div>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="mt-2"
                onClick={() => append({ key: "", value: "" })}
              >
                + Add goal
              </Button>
            </div>

            {createProject.error && (
              <p className="text-sm text-destructive">
                Failed to create project: {String(createProject.error)}
              </p>
            )}

            <div className="flex justify-end gap-2">
              <Button
                type="button"
                variant="outline"
                onClick={() => navigate({ to: "/projects" })}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={isSubmitting}>
                {isSubmitting ? "Creating…" : "Create Project"}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
