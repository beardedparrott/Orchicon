// Idea Cloud query and mutation hooks (TanStack Query + Connect-ES).
//
// Feature 5.2. Ideas are automation-produced work items in the
// system-managed `idea` status, awaiting human triage. This module wraps
// the three feature-5.1 sanctioned RPCs:
//   - ListIdeas      — page of idea-state items (scoped to idea by the server)
//   - PromoteIdea    — CAS idea -> pending (leaves Idea Cloud, becomes a
//                      normal work item)
//   - DismissIdea    — CAS idea -> cancelled (discarded)
//
// The "leaves Idea Cloud" behavior is entirely server-enforced (ListIdeas is
// scoped to status='idea' by construction), so no optimistic removal is done
// here — invalidating the idea + work-item keys makes the card disappear on
// the next refetch with server-confirmed state (invariant #3).

import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { workItemClient } from "@/api/clients";
import { workItemKeys } from "@/api/workItems";
import { IdeaStateScope } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";

// Query keys are centralized so invalidation is type-safe. Invalidation
// uses `ideaKeys.all` (["ideas"]) which is a prefix of every list key, so a
// promote/dismiss refetches whichever idea list is active.
export const ideaKeys = {
  all: ["ideas"] as const,
  list: (
    projectId: string,
    opts?: { search?: string; sortBy?: string; sortOrder?: string },
  ) => {
    const key: unknown[] = [...ideaKeys.all, "list", projectId];
    if (opts !== undefined) key.push(opts);
    return key;
  },
};

export interface ListIdeasOptions {
  search?: string;
  sortBy?: string;
  sortOrder?: string;
  // "active" (default) = idea-state items awaiting triage (the Idea Cloud);
  // "rejected" = previously dismissed idea spawns (the rejected section).
  state?: "active" | "rejected";
  pageSize?: number;
  refetchInterval?: number;
  enabled?: boolean;
}

// useListIdeas fetches a page of idea-state items for a project (empty
// projectId = all projects), optionally with free-text search + sort. It
// polls every 5s so freshly spawned ideas appear without a manual refresh
// (ideas arrive asynchronously from recurring fires). state="rejected"
// reads the rejected graveyard instead — dismissed spawns stay queryable
// as history (and as the memory the automation dedupe gate checks).
export function useListIdeas(projectId: string, opts?: ListIdeasOptions) {
  const listOpts = {
    search: opts?.search,
    sortBy: opts?.sortBy,
    sortOrder: opts?.sortOrder,
    state: opts?.state ?? "active",
  };
  return useQuery({
    queryKey: ideaKeys.list(projectId, listOpts),
    queryFn: async () => {
      const res = await workItemClient.listIdeas({
        projectId,
        search: opts?.search || "",
        sortBy: opts?.sortBy || "",
        sortOrder: opts?.sortOrder || "",
        ideaStateScope:
          opts?.state === "rejected"
            ? IdeaStateScope.REJECTED
            : IdeaStateScope.UNSPECIFIED,
        pageSize: opts?.pageSize ?? 1000,
      });
      return res.ideas as WorkItem[];
    },
    enabled: opts?.enabled ?? true,
    // keepPreviousData prevents the list flashing empty during refetches
    // after a promote/dismiss mutation.
    placeholderData: keepPreviousData,
    refetchInterval: opts?.refetchInterval ?? 5_000,
    refetchOnWindowFocus: true,
  });
}

// usePromoteIdea promotes an idea to a normal pending work item. On success
// it invalidates the idea lists (the card leaves Idea Cloud) and the
// work-item list/graph keys (the item appears in Work Items).
export function usePromoteIdea() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      const res = await workItemClient.promoteIdea({ id });
      return res.workItem as WorkItem;
    },
    onSuccess: (item) => {
      qc.invalidateQueries({ queryKey: ideaKeys.all });
      qc.invalidateQueries({ queryKey: workItemKeys.all });
      qc.invalidateQueries({ queryKey: workItemKeys.graph(item.projectId) });
    },
  });
}

// useDismissIdea discards an idea (CAS idea -> cancelled). On success it
// invalidates the idea lists — the card leaves the Idea Cloud AND appears
// in the Rejected section (the dismissed spawn keeps its provenance and is
// exactly what the REJECTED scope queries) — plus the work-item keys so
// the cancelled item's list views stay consistent.
export function useDismissIdea() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      const res = await workItemClient.dismissIdea({ id });
      return res.workItem as WorkItem;
    },
    onSuccess: (item) => {
      qc.invalidateQueries({ queryKey: ideaKeys.all });
      qc.invalidateQueries({ queryKey: workItemKeys.all });
      qc.invalidateQueries({ queryKey: workItemKeys.graph(item.projectId) });
    },
  });
}
