// Persistent permission policy query hooks (TanStack Query + Connect-ES).
//
// The policy is the operator's DURABLE deny/accept list, stored in a YAML
// file on the control plane. The FILE is the source of truth and the plane
// is its single writer: these hooks never patch a local copy, and every
// mutation returns the refreshed policy from the server. A hand-edit and a
// GUI change therefore cannot diverge.
//
// A deny entry carries `overridable: false` — the cue the UI must honour by
// saying a session grant CANNOT override it, rather than offering a grant
// that the refusal will ignore.

import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";

import { settingsClient } from "@/api/clients";
import type {
  PermissionPolicyEntry,
  GetPermissionPolicyResponse,
} from "@/api/gen/orchicon/api/v1/settings_service_pb";

export const permissionsKeys = {
  all: ["permission-policy"] as const,
};

/** The whole policy: the file path plus every entry (deny entries first). */
export function useGetPermissionPolicy() {
  return useQuery({
    queryKey: permissionsKeys.all,
    queryFn: async () => {
      const res = await settingsClient.getPermissionPolicy({});
      return res as GetPermissionPolicyResponse;
    },
  });
}

export function useAddPermissionPolicyEntry() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { pattern: string; list: "deny" | "accept" }) => {
      const res = await settingsClient.addPermissionPolicyEntry(input);
      return res.policy;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: permissionsKeys.all });
    },
  });
}

export function useRemovePermissionPolicyEntry() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { pattern: string; list: "deny" | "accept" }) => {
      const res = await settingsClient.removePermissionPolicyEntry(input);
      return res.policy;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: permissionsKeys.all });
    },
  });
}

/**
 * denyEntries / acceptEntries split the entry list by list name. The server
 * already orders deny-first, but the UI groups them, so it does not depend
 * on that ordering to be correct.
 */
export function denyEntries(entries: PermissionPolicyEntry[]): PermissionPolicyEntry[] {
  return entries.filter((e) => e.list === "deny");
}

export function acceptEntries(entries: PermissionPolicyEntry[]): PermissionPolicyEntry[] {
  return entries.filter((e) => e.list !== "deny");
}
