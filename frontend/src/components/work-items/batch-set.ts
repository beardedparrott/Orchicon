// Bulk-set `workflow_id` + `runtime_image` on the Work Items page.
//
// The bulk counterpart of the single-item form, and the remedy for "a work item with no workflow
// binding cannot run": it strands a whole backlog bound to nothing, and fixing that one modal at a
// time is the tedium this removes.
//
// `workflow_id` travels WITH `runtime_image` because the two are chosen together when building
// sequential work items — a step's container and the workflow that drives it are a pair.
//
// NO PROTO CHANGE AND NO NEW RPC. `UpdateWorkItemRequest.workflow_id` (field 15) and
// `.runtime_image` (field 19) are both `optional`: an UNSET key is left unchanged and `empty` is the
// documented unbind / base-image value. So the write is a `Promise.allSettled` of per-item updates,
// exactly like `batch-run.ts` / `batch-move.ts`, and clearing needs no extra mechanism.
//
// THE INDEPENDENCE GUARANTEE IS STRUCTURAL: `buildSetTarget` OMITS the untouched key entirely, so a
// workflow-only set physically cannot clobber a runtime image. Sending `""` for an untouched field
// would silently unbind it.

import { useCallback, useMemo, useState } from "react";
import { useQueries, useQueryClient } from "@tanstack/react-query";

import { workflowClient } from "@/api/clients";
import { useListWorkflows, workflowKeys } from "@/api/workflows";
import { useUpdateWorkItem, workItemKeys } from "@/api/workItems";
import { useToast } from "@/components/ui/toast";
import {
  WorkflowStatus,
  type Workflow,
  type WorkflowVersion,
} from "@/api/gen/orchicon/api/v1/workflow_pb";
import type { PartialMessage } from "@bufbuild/protobuf";
import type { UpdateWorkItemRequest } from "@/api/gen/orchicon/api/v1/work_item_service_pb";

/** The "leave unchanged" sentinel. No real workflow id or image tag can equal it. */
export const SET_SKIP = "__skip__";

/** The documented clearing value: empty workflow_id = unbind; empty runtime_image = base image. */
export const SET_CLEAR = "";

/** What to write. An ABSENT key means "leave that field alone". */
export interface SetTarget {
  workflowId?: string;
  runtimeImage?: string;
}

/**
 * Build the partial update from the two picker selections.
 *
 * The untouched key is OMITTED (not sent as empty): `UpdateWorkItemRequest`'s fields are
 * `optional`, so an unset key is left unchanged. This is what makes AC2 — the independence of the
 * two operations — structural rather than hoped for.
 */
export function buildSetTarget(wfSel: string, imgSel: string): SetTarget {
  const target: SetTarget = {};
  if (wfSel !== SET_SKIP) target.workflowId = wfSel;
  if (imgSel !== SET_SKIP) target.runtimeImage = imgSel;
  return target;
}

/**
 * The predicate the SERVER applies — but only at SCHEDULE time (`ValidateSequenceSubtree`:
 * status PUBLISHED|DEPRECATED AND the workflow has >= 1 step).
 *
 * `UpdateWorkItem` performs NO bind-time validation of `workflow_id`, so a bulk bind of a
 * non-runnable template would land silently and only fail later, when the item is scheduled. The
 * picker therefore applies the schedule-time predicate itself and EXCLUDES non-runnable templates.
 */
export function isRunnableWorkflow(
  wf: Workflow | undefined,
  latest: WorkflowVersion | undefined,
): boolean {
  if (!wf) return false;
  const statusOk =
    wf.status === WorkflowStatus.PUBLISHED || wf.status === WorkflowStatus.DEPRECATED;
  if (!statusOk) return false;
  const steps = (latest?.steps ?? "").trim();
  if (steps === "") return false;
  try {
    const parsed: unknown = JSON.parse(steps);
    return Array.isArray(parsed) && parsed.length > 0;
  } catch {
    // A malformed steps document cannot be shown to be runnable — refuse it rather than offer a
    // target that will fail asynchronously.
    return false;
  }
}

/**
 * The partial-failure wording, in the house style (`batch-run.ts` / bulk delete): a run that fails
 * on some items must say so with a COUNT, never read as a clean sweep.
 */
export function formatSetResult(
  applied: number,
  total: number,
  failures: { title: string; reason: string }[],
): string {
  const head = `set ${applied} of ${total}`;
  if (failures.length === 0) return `${head} — all applied`;
  const detail = failures.map((f) => `"${f.title}": ${f.reason}`).join(", ");
  return `${head} — ${failures.length} rejected: ${detail}`;
}

/**
 * The confirm text: it names the COUNT and the VALUE(S) being applied, and — when the selection
 * contains a sequence parent — says the parent's own binding is INERT.
 *
 * The parent note is not decoration: a parent-with-children is a sequence container, so even a
 * parent carrying a fresh binding is routed to the sequence engine at fire time and its children
 * each run their own workflows. Setting it is still allowed (the single-item form permits it too),
 * but the operator must not believe they routed the parent.
 */
export function bulkSetConfirmText(
  count: number,
  target: SetTarget,
  workflowLabel: string,
  parentCount: number,
): string {
  const values: string[] = [];
  if ("workflowId" in target) {
    values.push(
      target.workflowId === SET_CLEAR
        ? "workflow: cleared (unbound)"
        : `workflow: ${workflowLabel || target.workflowId}`,
    );
  }
  if ("runtimeImage" in target) {
    values.push(
      target.runtimeImage === SET_CLEAR
        ? "runtime image: cleared (base image)"
        : `runtime image: ${target.runtimeImage}`,
    );
  }
  const noun = count === 1 ? "work item" : "work items";
  let text = `Set ${values.join(", ") || "nothing"} on ${count} ${noun}?`;
  if (parentCount > 0) {
    text +=
      `\n\nNote: ${parentCount} of the selection ${parentCount === 1 ? "is a SEQUENCE PARENT" : "are SEQUENCE PARENTS"} — ` +
      "a parent's own workflow/image binding is INERT; its children each run their own workflows. " +
      "The value is stored anyway and does not route the parent.";
  }
  return text;
}

/** The workflow TEMPLATES that can actually run, plus how many were hidden and why. */
export function useRunnableWorkflowOptions(): {
  options: Workflow[];
  hidden: number;
  isLoading: boolean;
} {
  const list = useListWorkflows({ templatesOnly: true });
  const candidates = useMemo(
    () =>
      (list.data ?? []).filter(
        (w) =>
          w.status === WorkflowStatus.PUBLISHED ||
          w.status === WorkflowStatus.DEPRECATED,
      ),
    [list.data],
  );
  // The step count only lives on the version, and the `Workflow` message carries none — so each
  // candidate needs one GetWorkflow. The query key is `workflowKeys.detail(id)`, the SAME key
  // `useGetWorkflow` uses, so the version is fetched once and shared with the rest of the page
  // rather than cached twice.
  const versions = useQueries({
    queries: candidates.map((w) => ({
      queryKey: workflowKeys.detail(w.id),
      queryFn: async () => {
        const res = await workflowClient.getWorkflow({ id: w.id });
        return {
          workflow: res.workflow as Workflow,
          latestVersion: (res.latestVersion ?? undefined) as WorkflowVersion | undefined,
        };
      },
    })),
  });

  const options = useMemo(() => {
    const out: Workflow[] = [];
    candidates.forEach((w, i) => {
      const v = versions[i]?.data?.latestVersion;
      if (isRunnableWorkflow(w, v)) out.push(w);
    });
    return out;
  }, [candidates, versions]);

  // `hidden` counts only the ones we have PROVEN cannot run: a status that is not
  // PUBLISHED/DEPRECATED, a version fetch that failed, or a version with no steps. A candidate whose
  // version is still IN FLIGHT is NOT counted — the dialog renders this as "N workflows hidden — not
  // published, or has no steps. Binding one would produce an item that cannot run", and saying that
  // about a template that merely had not loaded yet is a false claim, not a cosmetic one.
  const hidden = useMemo(() => {
    // Status is known from the list itself, before any version is fetched.
    let n = (list.data?.length ?? 0) - candidates.length;
    candidates.forEach((w, i) => {
      const q = versions[i];
      if (!q || q.isPending) return;
      if (q.isError || !isRunnableWorkflow(w, q.data?.latestVersion)) n += 1;
    });
    return n;
  }, [list.data, candidates, versions]);

  return {
    options,
    hidden,
    isLoading: list.isLoading,
  };
}

/**
 * Applies a target to a set of work items. Mirrors `useBatchRunWorkItems`: `Promise.allSettled` of
 * the per-item update, ONE summary toast, and a refetch when anything was rejected so the cache
 * shows the truth (there is no optimistic write — a binding is server state).
 */
export function useBatchSetWorkItems(projectId: string) {
  const qc = useQueryClient();
  const toast = useToast();
  const updateWorkItem = useUpdateWorkItem(projectId);
  const [pendingCount, setPendingCount] = useState(0);

  const setSelected = useCallback(
    async (
      ids: string[],
      target: SetTarget,
      titles?: Map<string, string>,
    ): Promise<{ applied: number; failures: { title: string; reason: string }[] }> => {
      if (ids.length === 0) return { applied: 0, failures: [] };
      if (Object.keys(target).length === 0) {
        toast.error("Choose a workflow or a runtime image to set (or to clear).", {
          title: "Nothing to set",
        });
        return { applied: 0, failures: [] };
      }

      setPendingCount((c) => c + 1);
      try {
        const results = await Promise.allSettled(
          ids.map((id) =>
            updateWorkItem.mutateAsync({ id, ...target } as PartialMessage<UpdateWorkItemRequest>),
          ),
        );
        const failures: { title: string; reason: string }[] = [];
        results.forEach((r, i) => {
          if (r.status === "rejected") {
            const id = ids[i];
            failures.push({
              title: titles?.get(id) ?? id,
              reason: r.reason instanceof Error ? r.reason.message : String(r.reason),
            });
          }
        });
        if (failures.length > 0) {
          qc.invalidateQueries({ queryKey: workItemKeys.list(projectId) });
        }
        const applied = ids.length - failures.length;
        const msg = formatSetResult(applied, ids.length, failures);
        if (applied === 0) toast.error(msg, { title: "Nothing set" });
        else toast.success(msg);
        return { applied, failures };
      } finally {
        setPendingCount((c) => Math.max(0, c - 1));
      }
    },
    [projectId, qc, toast, updateWorkItem],
  );

  return { setSelected, isPending: pendingCount > 0 };
}
