// AuthService query + mutation hooks (TanStack Query + Connect-ES).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { authClient } from "@/api/clients";

export const authKeys = {
  tenants: () => ["auth", "tenants"] as const,
  identities: () => ["auth", "identities"] as const,
  roles: () => ["auth", "roles"] as const,
  apiKeys: () => ["auth", "apiKeys"] as const,
  entitlements: (id: string) => ["auth", "entitlements", id] as const,
  audit: () => ["auth", "audit"] as const,
};

export function useListTenants() {
  return useQuery({
    queryKey: authKeys.tenants(),
    queryFn: async () => (await authClient.listTenants({ pageSize: 100 })).tenants ?? [],
  });
}

export function useListIdentities() {
  return useQuery({
    queryKey: authKeys.identities(),
    queryFn: async () => (await authClient.listIdentities({ pageSize: 100 })).identities ?? [],
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
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.roles() }),
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

export function useListAuditEntries() {
  return useQuery({
    queryKey: authKeys.audit(),
    queryFn: async () =>
      (await authClient.listAuditEntries({ pageSize: 100 })).entries ?? [],
  });
}
