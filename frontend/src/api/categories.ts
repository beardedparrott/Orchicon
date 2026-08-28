import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { getAccessToken } from "@/auth/session";

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

const BASE = typeof window !== "undefined" ? window.location.origin : "http://localhost:8080";

function authHeaders(): Record<string, string> {
  const token = getAccessToken();
  const h: Record<string, string> = { "Content-Type": "application/json" };
  if (token) h["Authorization"] = `Bearer ${token}`;
  return h;
}

async function connectPost<T>(service: string, method: string, body: unknown): Promise<T> {
  const res = await fetch(`${BASE}/${service}/${method}`, {
    method: "POST",
    headers: authHeaders(),
    credentials: "include",
    body: JSON.stringify(body ?? {}),
  });
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(text || `${method} failed: ${res.status}`);
  }
  const json = await res.json().catch(() => ({}));
  return json as T;
}

export const categoryKeys = {
  all: ["categories"] as const,
  list: (targetType: CategoryTargetType) => ["categories", "list", targetType] as const,
  assignments: (targetType: CategoryTargetType) => ["categories", "assignments", targetType] as const,
};

export function useListCategories(targetType: CategoryTargetType) {
  return useQuery({
    queryKey: categoryKeys.list(targetType),
    queryFn: async () => {
      const r = await connectPost<{ categories: CategoryDTO[] }>("orchicon.api.v1.CategoryService", "ListCategories", { targetType });
      const list = (r as unknown as { categories?: CategoryDTO[] })?.categories ?? (r as unknown as CategoryDTO[]) ?? [];
      return Array.isArray(list) ? list : [];
    },
  });
}

export function useListAssignments(targetType: CategoryTargetType) {
  return useQuery({
    queryKey: categoryKeys.assignments(targetType),
    queryFn: async () => {
      const r = await connectPost<{ assignments: CategoryAssignmentDTO[] } | CategoryAssignmentDTO[]>("orchicon.api.v1.CategoryService", "ListAssignments", { targetType });
      const list = (r as unknown as { assignments?: CategoryAssignmentDTO[] })?.assignments ?? (r as unknown as CategoryAssignmentDTO[]) ?? [];
      return Array.isArray(list) ? (list as CategoryAssignmentDTO[]) : [];
    },
  });
}

export function useCreateCategory(targetType: CategoryTargetType) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { name: string; description?: string }) => {
      return connectPost<CategoryDTO>("orchicon.api.v1.CategoryService", "CreateCategory", { targetType, name: input.name, description: input.description ?? "" });
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
      return connectPost<CategoryDTO>("orchicon.api.v1.CategoryService", "UpdateCategory", { targetType, id: input.id, name: input.name, description: input.description });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: categoryKeys.list(targetType) }),
  });
}

export function useDeleteCategory(targetType: CategoryTargetType) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      return connectPost("orchicon.api.v1.CategoryService", "DeleteCategory", { targetType, id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: categoryKeys.list(targetType) });
      qc.invalidateQueries({ queryKey: categoryKeys.assignments(targetType) });
    },
  });
}

export function useAssignToCategory(targetType: CategoryTargetType) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { categoryId: string; entityId: string }) => {
      return connectPost("orchicon.api.v1.CategoryService", "AssignToCategory", { targetType, categoryId: input.categoryId, entityId: input.entityId });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: categoryKeys.assignments(targetType) }),
  });
}

export function useUnassignFromCategory(targetType: CategoryTargetType) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (entityId: string) => {
      return connectPost("orchicon.api.v1.CategoryService", "UnassignFromCategory", { targetType, entityId });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: categoryKeys.assignments(targetType) }),
  });
}

export function useReorderCategories(targetType: CategoryTargetType) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (orderedIds: string[]) => {
      return connectPost("orchicon.api.v1.CategoryService", "ReorderCategories", { targetType, orderedIds });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: categoryKeys.list(targetType) }),
  });
}
