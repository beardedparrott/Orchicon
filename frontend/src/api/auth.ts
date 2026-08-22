// AuthService query + mutation hooks (TanStack Query + Connect-ES).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { Timestamp } from "@bufbuild/protobuf";

import { authClient } from "@/api/clients";

export const authKeys = {
  tenants: () => ["auth", "tenants"] as const,
  identities: () => ["auth", "identities"] as const,
  roles: () => ["auth", "roles"] as const,
  roleBindings: () => ["auth", "roleBindings"] as const,
  apiKeys: () => ["auth", "apiKeys"] as const,
  entitlements: (id: string) => ["auth", "entitlements", id] as const,
  audit: () => ["auth", "audit"] as const,
  auditEvents: (filters?: AuditEventFilters) =>
    [
      "auth",
      "auditEvents",
      filters?.action ?? "",
      filters?.actorId ?? "",
      filters?.targetType ?? "",
      filters?.targetId ?? "",
      filters?.startTime ? Number(filters.startTime.seconds) : "",
      filters?.endTime ? Number(filters.endTime.seconds) : "",
    ] as const,
};

export function useListTenants() {
  return useQuery({
    queryKey: authKeys.tenants(),
    queryFn: async () => (await authClient.listTenants({ pageSize: 100 })).tenants ?? [],
  });
}

export function useCreateTenant() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      slug: string;
      name: string;
      budgetEnvelopeJson?: string;
    }) => (await authClient.createTenant(input)).tenant,
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.tenants() }),
  });
}

export function useListIdentities() {
  return useQuery({
    queryKey: authKeys.identities(),
    queryFn: async () => (await authClient.listIdentities({ pageSize: 100 })).identities ?? [],
  });
}

export function useCreateIdentity() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      identityType: string;
      subject?: string;
      displayName: string;
    }) => (await authClient.createIdentity(input)).identity,
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.identities() }),
  });
}

export function useUpdateIdentity() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      id: string;
      displayName: string;
      version?: number;
    }) => (await authClient.updateIdentity(input)).identity,
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.identities() }),
  });
}

export function useSetIdentityStatus() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      id: string;
      status: string;
      version?: number;
    }) => (await authClient.setIdentityStatus(input)).identity,
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.identities() }),
  });
}

export function useDeleteIdentity() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await authClient.deleteIdentity({ id });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.identities() }),
  });
}

export function useListRoles() {
  return useQuery({
    queryKey: authKeys.roles(),
    queryFn: async () => (await authClient.listRoles({ pageSize: 100 })).roles ?? [],
  });
}

export function useCreateRole() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      name: string;
      scope: string;
      scopeRef?: string;
      entitlements: string[];
    }) => (await authClient.createRole(input)).role,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: authKeys.roles() });
      qc.invalidateQueries({ queryKey: authKeys.roleBindings() });
    },
  });
}

export function useUpdateRole() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      id: string;
      name?: string;
      entitlements?: string[];
      version?: number;
    }) =>
      (await authClient.updateRole({
        id: input.id,
        name: input.name,
        entitlements: input.entitlements !== undefined
          ? { values: input.entitlements }
          : undefined,
        version: input.version,
      })).role,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: authKeys.roles() });
      qc.invalidateQueries({ queryKey: authKeys.roleBindings() });
    },
  });
}

export function useDeleteRole() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await authClient.deleteRole({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: authKeys.roles() });
      qc.invalidateQueries({ queryKey: authKeys.roleBindings() });
    },
  });
}

export function useListRoleBindings() {
  return useQuery({
    queryKey: authKeys.roleBindings(),
    queryFn: async () =>
      (await authClient.listRoleBindings({ pageSize: 100 })).bindings ?? [],
  });
}

export function useAssignRole() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      identityId: string;
      roleId: string;
      scope?: string;
      scopeRef?: string;
    }) => (await authClient.assignRole(input)).binding,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: authKeys.roles() });
      qc.invalidateQueries({ queryKey: authKeys.identities() });
      qc.invalidateQueries({ queryKey: authKeys.roleBindings() });
    },
  });
}

export function useRevokeRole() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await authClient.revokeRole({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: authKeys.roles() });
      qc.invalidateQueries({ queryKey: authKeys.identities() });
      qc.invalidateQueries({ queryKey: authKeys.roleBindings() });
    },
  });
}

export function useListApiKeys() {
  return useQuery({
    queryKey: authKeys.apiKeys(),
    queryFn: async () => (await authClient.listApiKeys({ pageSize: 100 })).apiKeys ?? [],
  });
}

export function useCreateApiKey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      identityId: string;
      name: string;
      scopes: string[];
    }) => await authClient.createApiKey(input),
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.apiKeys() }),
  });
}

export function useRevokeApiKey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => (await authClient.revokeApiKey({ id })).apiKey,
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.apiKeys() }),
  });
}

export function useRotateApiKey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => await authClient.rotateApiKey({ id }),
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.apiKeys() }),
  });
}

export function useListEntitlements(identityId: string) {
  return useQuery({
    queryKey: authKeys.entitlements(identityId),
    queryFn: async () => (await authClient.listEntitlements({ identityId })),
    enabled: !!identityId,
  });
}

export function useSetLocalCredential() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      identityId: string;
      username: string;
      password: string;
    }) => (await authClient.setLocalCredential(input)).username,
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.identities() }),
  });
}

export function useListAuditEntries() {
  return useQuery({
    queryKey: authKeys.audit(),
    queryFn: async () =>
      (await authClient.listAuditEntries({ pageSize: 100 })).entries ?? [],
  });
}

// AuditEventFilters scopes useListAuditEvents. All fields optional;
// startTime is an inclusive lower bound, endTime an exclusive upper
// bound on occurred_at (absent = unbounded).
export interface AuditEventFilters {
  action?: string;
  actorId?: string;
  targetType?: string;
  targetId?: string;
  startTime?: Timestamp;
  endTime?: Timestamp;
}

// useListAuditEvents fetches a page of audit_events rows (the
// actor-based trail written by internal/audit.Record — distinct from the
// policy-decision AuditEntry view). The query is keyed on the filters so
// each filter combination refetches independently.
export function useListAuditEvents(filters?: AuditEventFilters) {
  return useQuery({
    queryKey: authKeys.auditEvents(filters),
    queryFn: async () =>
      (
        await authClient.listAuditEvents({
          pageSize: 200,
          action: filters?.action ?? undefined,
          actorId: filters?.actorId ?? undefined,
          targetType: filters?.targetType ?? undefined,
          targetId: filters?.targetId ?? undefined,
          startTime: filters?.startTime,
          endTime: filters?.endTime,
        })
      ).events ?? [],
  });
}
