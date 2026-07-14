import { zodResolver } from "@hookform/resolvers/zod";
import { createRoute, useNavigate } from "@tanstack/react-router";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { useCreateProject } from "@/api/projects";
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

// Create project form (docs/10 §5, §2: React Hook Form + Zod).
//
// Zod validation mirrors the server-side rules (internal/project/validate.go)
// so the form rejects invalid input before round-tripping: name is
// required and bounded, slug is optional and slug-regex-constrained, and
// goals must be valid JSON if provided.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/new",
  component: NewProjectPage,
});

const slugRegex = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

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
  goals: z
    .string()
    .max(1_048_576, "Goals document is too large")
    .refine((v) => v === "" || v === undefined || isValidJson(v), {
      message: "Goals must be valid JSON",
    })
    .optional(),
});

type CreateProjectForm = z.infer<typeof createProjectSchema>;

function NewProjectPage() {
  const navigate = useNavigate();
  const createProject = useCreateProject();
  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<CreateProjectForm>({
    resolver: zodResolver(createProjectSchema),
    defaultValues: { name: "", slug: "", goals: "" },
  });

  const onSubmit = async (values: CreateProjectForm) => {
    const project = await createProject.mutateAsync({
      name: values.name,
      slug: values.slug || undefined,
      goals: values.goals || undefined,
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
              <Label htmlFor="goals">Goals (optional JSON)</Label>
              <Textarea
                id="goals"
                placeholder='{"summary": "Build and ship the MVP"}'
                rows={5}
                {...register("goals")}
              />
              {errors.goals && (
                <p className="text-xs text-destructive">
                  {errors.goals.message}
                </p>
              )}
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

function isValidJson(s: string): boolean {
  try {
    JSON.parse(s);
    return true;
  } catch {
    return false;
  }
}
