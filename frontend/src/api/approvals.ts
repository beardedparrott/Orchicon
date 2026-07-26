// Approval hooks (TanStack Query + Connect-ES).
// Wraps the generated ApprovalService client with query keys and mutations.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { approvalClient } from "@/api/clients";
import type { ApprovalItem } from "@/api/gen/orchicon/api/v1/approval_service_pb";
import type { PartialMessage } from "@bufbuild/protobuf";

export const approvalKeys = {
  all: ["approvals"] as const,
  list: (workflowRunId?: string, status?: string) =>
    [...approvalKeys.all, "list", workflowRunId, status] as const,
};

export function useListPendingStepApprovals(opts?: {
  workflowRunId?: string;
  status?: string;
  search?: string;
  sortBy?: string;
  sortOrder?: string;
  enabled?: boolean;
}) {
  return useQuery({
    queryKey: approvalKeys.list(opts?.workflowRunId, opts?.status),
    queryFn: async () => {
      const res = await approvalClient.listPendingStepApprovals({
        workflowRunId: opts?.workflowRunId ?? undefined,
        status: opts?.status ?? undefined,
        search: opts?.search ?? "",
        sortBy: opts?.sortBy ?? "",
        sortOrder: opts?.sortOrder ?? "",
      });
      return res.items as ApprovalItem[];
    },
    enabled: opts?.enabled ?? true,
    refetchInterval: (query) => {
      const items = query.state.data;
      if (!items) return 5000;
      return items.some((i) => i.status === "approval_pending" || i.status === "pending") ? 3000 : false;
    },
  });
}

export function useApproveStep() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (req: PartialMessage<{
      stepRunId: string;
      approved: boolean;
      reason: string;
      reviewedBy: string;
    }>) => {
      const res = await approvalClient.approveStep({
        stepRunId: req.stepRunId!,
        approved: req.approved!,
        reason: req.reason ?? "",
        reviewedBy: req.reviewedBy ?? "",
      });
      return res;
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: approvalKeys.all });
    },
  });
}
