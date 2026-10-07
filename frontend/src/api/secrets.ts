// Tenant secrets as a CLIENT sees them: names, ids and descriptions, never a value.
//
// The store is write-only from this side on purpose (the same rule the Settings → Secrets tab and the
// Control screen's list keep): ListSecrets returns metadata, GetSecret is never called, and a credential
// is written once and resolved plane-side — for MCP servers that is
// internal/mcpsettings.ResolveSecretRefs, which substitutes the plaintext into a server's env/headers at
// session time and never stores it there.
//
// It exists as hooks (rather than the ad-hoc `useEffect` + client calls a couple of screens use) because
// the MCP panels need the list in three places: to OFFER a credential that /exists/, and to decide
// whether a value has to be STORED under a new name or REPLACED under an existing one.
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";

import { secretsClient } from "@/api/clients";

export const secretKeys = {
  all: ["tenant-secrets"] as const,
};

export interface TenantSecretMeta {
  id: string;
  name: string;
  description: string;
}

// useSecretList reads the tenant's stored secret NAMES.
export function useSecretList() {
  return useQuery({
    queryKey: secretKeys.all,
    queryFn: async (): Promise<TenantSecretMeta[]> => {
      const res = await secretsClient.listSecrets({ pageSize: 200 });
      return (res.secrets ?? []).map((s) => ({
        id: s.id,
        name: s.name,
        description: s.description ?? "",
      }));
    },
  });
}

// useCreateSecret stores a new credential under a name the caller chose.
export function useCreateSecret() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: { name: string; value: string; description?: string }) =>
      secretsClient.createSecret(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: secretKeys.all }),
  });
}

// useUpdateSecret replaces an EXISTING credential's value, by id (a rotated token). It takes an id
// rather than a name because the store's key is the id — the name is what a ${REFERENCE} uses.
export function useUpdateSecret() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: { id: string; value: string }) =>
      secretsClient.updateSecret({ id: req.id, value: req.value }),
    onSuccess: () => qc.invalidateQueries({ queryKey: secretKeys.all }),
  });
}
