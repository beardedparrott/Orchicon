import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { categoryClient } from "@/api/clients";
import { CategoryTargetType as ProtoTargetType } from "@/api/gen/orchicon/api/v1/category_pb";

export type CategoryTargetType = "worker" | "workflow" | "conversation";

export interface CategoryDTO {
  id: string;
  targetType: CategoryTargetType;
  name: string;
  slug: string;
  description: string;
  sortOrder: number;
  createdAt?: string;
  updatedAt?: string;
}

export interface CategoryAssignmentDTO {
  categoryId: string;
  entityId: string;
  targetType: CategoryTargetType;
}

function toProtoTarget(t: CategoryTargetType): ProtoTargetType {
  switch (t) {
    case "worker": return ProtoTargetType.WORKER;
    case "workflow": return ProtoTargetType.WORKFLOW;
    case "conversation": return ProtoTargetType.CONVERSATION;
  }
}

function fromProtoTarget(t: ProtoTargetType): CategoryTargetType {
  switch (t) {
    case ProtoTargetType.WORKER: return "worker";
    case ProtoTargetType.WORKFLOW: return "workflow";
    case ProtoTargetType.CONVERSATION: return "conversation";
    default: return "worker";
  }
}

export const categoryKeys = {
  all: ["categories"] as const,
  list: (targetType: CategoryTargetType) => ["categories", "list", targetType] as const,
};

export interface CategoryListResult {
  categories: CategoryDTO[];
  assignments: CategoryAssignmentDTO[];
}

function mapCategory(c: { id: string; targetType: ProtoTargetType; name: string; slug: string; description: string; sortOrder: number; createdAt?: unknown; updatedAt?: unknown }): CategoryDTO {
  return {
    id: c.id,
    targetType: fromProtoTarget(c.targetType),
    name: c.name,
    slug: c.slug,
    description: c.description,
    sortOrder: c.sortOrder,
    createdAt: (c.createdAt as { toDate?: () => Date })?.toDate?.()?.toISOString(),
    updatedAt: (c.updatedAt as { toDate?: () => Date })?.toDate?.()?.toISOString(),
  };
}

export function useListCategories(targetType: CategoryTargetType) {
  return useQuery({
    queryKey: categoryKeys.list(targetType),
    queryFn: async (): Promise<CategoryListResult> => {
      const res = await categoryClient.listCategories({ targetType: toProtoTarget(targetType) });
      const categories: CategoryDTO[] = (res.categories ?? []).map((c) => mapCategory(c as unknown as { id: string; targetType: ProtoTargetType; name: string; slug: string; description: string; sortOrder: number }));
      const assignments: CategoryAssignmentDTO[] = (res.assignments ?? []).map((a) => ({
        categoryId: a.categoryId,
        entityId: a.entityId,
        targetType: fromProtoTarget(a.targetType),
      }));
      return { categories, assignments };
    },
  });
}

// Backwards-compat: some callers import useListAssignments; keep it as alias to the single source.
export function useListAssignments(targetType: CategoryTargetType) {
  const q = useListCategories(targetType);
  return {
    ...q,
    data: q.data?.assignments,
  } as unknown as ReturnType<typeof useQuery<CategoryAssignmentDTO[]>>;
}

export function useCreateCategory(targetType: CategoryTargetType) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { name: string; description?: string }) => {
      const res = await categoryClient.createCategory({ targetType: toProtoTarget(targetType), name: input.name, description: input.description ?? "" });
      return res.category ? mapCategory(res.category as unknown as { id: string; targetType: ProtoTargetType; name: string; slug: string; description: string; sortOrder: number }) : null;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: categoryKeys.list(targetType) });
    },
  });
}

export function useUpdateCategory(targetType: CategoryTargetType) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { id: string; name?: string; description?: string }) => {
      const res = await categoryClient.updateCategory({ id: input.id, name: input.name, description: input.description });
      return res.category ? mapCategory(res.category as unknown as { id: string; targetType: ProtoTargetType; name: string; slug: string; description: string; sortOrder: number }) : null;
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: categoryKeys.list(targetType) }),
  });
}

export function useDeleteCategory(targetType: CategoryTargetType) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await categoryClient.deleteCategory({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: categoryKeys.list(targetType) });
    },
  });
}

export function useAssignToCategory(targetType: CategoryTargetType) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { categoryId: string; entityId: string }) => {
      await categoryClient.assignToCategory({ categoryId: input.categoryId, entityId: input.entityId, targetType: toProtoTarget(targetType) });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: categoryKeys.list(targetType) }),
  });
}

export function useUnassignFromCategory(targetType: CategoryTargetType) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (entityId: string) => {
      await categoryClient.unassignFromCategory({ entityId, targetType: toProtoTarget(targetType) });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: categoryKeys.list(targetType) }),
  });
}

export function useReorderCategories(targetType: CategoryTargetType) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (orderedIds: string[]) => {
      await categoryClient.reorderCategories({ targetType: toProtoTarget(targetType), orderedIds });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: categoryKeys.list(targetType) }),
  });
}
