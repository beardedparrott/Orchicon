// Policy query and mutation hooks (TanStack Query + Connect-ES, docs/07
// §3.5). The policy editor's draft canvas state (unsaved Rego module)
// is local; save = UpdatePolicyVersion. The "test" pane calls
// EvaluatePolicy (dry-run). The decision log renders ListDecisions with
// drill-down to the Rego trace via ExplainDecision.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { policyClient } from "@/api/clients";
import type { PartialMessage } from "@bufbuild/protobuf";
import type { Policy } from "@/api/gen/orchicon/api/v1/policy_pb";
import type { PolicyVersion } from "@/api/gen/orchicon/api/v1/policy_pb";
import type { PolicyDecision } from "@/api/gen/orchicon/api/v1/policy_pb";
import type { DecisionPoint } from "@/api/gen/orchicon/api/v1/policy_pb";
import type { PolicyStatus } from "@/api/gen/orchicon/api/v1/policy_pb";
import type {
  CreatePolicyRequest,
  EvaluatePolicyRequest,
  UpdatePolicyVersionRequest,
} from "@/api/gen/orchicon/api/v1/policy_service_pb";

export const policyKeys = {
  all: ["policies"] as const,
  list: (decisionPoint?: DecisionPoint, status?: PolicyStatus) =>
    [...policyKeys.all, "list", decisionPoint, status] as const,
  detail: (id: string) => [...policyKeys.all, "detail", id] as const,
  versions: (id: string) => [...policyKeys.all, "versions", id] as const,
  decisions: (decisionPoint?: DecisionPoint, targetType?: string, targetId?: string) =>
    [...policyKeys.all, "decisions", decisionPoint, targetType, targetId] as const,
  decision: (id: string) => [...policyKeys.all, "decision", id] as const,
};

export function useListPolicies(opts?: { decisionPoint?: DecisionPoint; status?: PolicyStatus }) {
  return useQuery({
    queryKey: policyKeys.list(opts?.decisionPoint, opts?.status),
    queryFn: async () => {
      const res = await policyClient.listPolicies({
        pageSize: 100,
        decisionPoint: opts?.decisionPoint ?? undefined,
        status: opts?.status ?? undefined,
      });
      return res.policies as Policy[];
    },
  });
}

export function useGetPolicy(id: string) {
  return useQuery({
    queryKey: policyKeys.detail(id),
    queryFn: async () => {
      const res = await policyClient.getPolicy({ id });
      return {
        policy: res.policy as Policy,
        latestVersion: (res.latestVersion ?? undefined) as PolicyVersion | undefined,
      };
    },
    enabled: !!id,
  });
}

export function useListPolicyVersions(policyId: string) {
  return useQuery({
    queryKey: policyKeys.versions(policyId),
    queryFn: async () => {
      const res = await policyClient.listPolicyVersions({ policyId });
      return res.versions as PolicyVersion[];
    },
    enabled: !!policyId,
  });
}

export function useCreatePolicy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: PartialMessage<CreatePolicyRequest>) => {
      const res = await policyClient.createPolicy(input);
      return { policy: res.policy as Policy, version: res.version as PolicyVersion };
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: policyKeys.list() }),
  });
}

export function useUpdatePolicyVersion() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: PartialMessage<UpdatePolicyVersionRequest>) => {
      const res = await policyClient.updatePolicyVersion(input);
      return res.version as PolicyVersion;
    },
    onSuccess: (version) => {
      qc.invalidateQueries({ queryKey: policyKeys.versions(version.policyId) });
      qc.invalidateQueries({ queryKey: policyKeys.detail(version.policyId) });
    },
  });
}

export function usePublishPolicy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { policyId: string; versionNote?: string }) => {
      const res = await policyClient.publishPolicy(input);
      return { policy: res.policy as Policy, version: res.version as PolicyVersion };
    },
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: policyKeys.list() });
      qc.invalidateQueries({ queryKey: policyKeys.detail(data.policy.id) });
      qc.invalidateQueries({ queryKey: policyKeys.versions(data.policy.id) });
    },
  });
}

export function useSupersedePolicy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (policyId: string) => {
      const res = await policyClient.supersedePolicy({ policyId });
      return res.policy as Policy;
    },
    onSuccess: (policy) => {
      qc.invalidateQueries({ queryKey: policyKeys.list() });
      qc.invalidateQueries({ queryKey: policyKeys.detail(policy.id) });
    },
  });
}

// EvaluatePolicy (dry-run) — returns effect + Rego trace. Used by the
// policy editor's "test" pane.
export function useEvaluatePolicy() {
  return useMutation({
    mutationFn: async (input: PartialMessage<EvaluatePolicyRequest>) => {
      const res = await policyClient.evaluatePolicy(input);
      return {
        effect: res.effect,
        policyId: res.policyId,
        policyVersion: res.policyVersion,
        result: res.result,
        trace: res.trace,
        error: res.error,
      };
    },
  });
}

export function useListDecisions(opts?: {
  decisionPoint?: DecisionPoint;
  targetType?: string;
  targetId?: string;
  policyId?: string;
}) {
  return useQuery({
    queryKey: policyKeys.decisions(opts?.decisionPoint, opts?.targetType, opts?.targetId),
    queryFn: async () => {
      const res = await policyClient.listDecisions({
        pageSize: 100,
        decisionPoint: opts?.decisionPoint ?? undefined,
        targetType: opts?.targetType ?? "",
        targetId: opts?.targetId ?? "",
        policyId: opts?.policyId ?? "",
      });
      return res.decisions as PolicyDecision[];
    },
  });
}

export function useExplainDecision(decisionId: string) {
  return useQuery({
    queryKey: policyKeys.decision(decisionId),
    queryFn: async () => {
      const res = await policyClient.explainDecision({ decisionId });
      return (res.decision ?? undefined) as PolicyDecision | undefined;
    },
    enabled: !!decisionId,
  });
}
