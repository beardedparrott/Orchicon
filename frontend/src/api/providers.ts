// Provider settings hooks (ADR-0006): the tenant-facing Providers surface
// behind Settings → Adapters. All mutations invalidate the shared
// ["providers"] query key so every consumer — including the ModelPicker's
// provider tier — auto-refreshes on save.
import { useMemo } from "react";

import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";

import { providerClient } from "@/api/clients";
import type { ProviderEntry, ProviderModel } from "@/api/gen/orchicon/api/v1/provider_pb";
import { OpenCodeModel } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";

export const providersKeys = {
  all: ["providers"] as const,
  models: (providerId: string) => ["providers", "models", providerId] as const,
};

// useProviderList fetches the merged provider view (built-in read-only
// entries ⊕ stored overrides ⊕ tenant custom providers).
export function useProviderList() {
  return useQuery({
    queryKey: providersKeys.all,
    queryFn: async () => {
      const res = await providerClient.listProviders({});
      return (res.providers ?? []) as ProviderEntry[];
    },
  });
}

// useUpdateProviderSettings persists partial provider settings (enable/
// disable, baseURL override, Ollama num_ctx, hidden models).
export function useUpdateProviderSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: Parameters<typeof providerClient.updateProviderSettings>[0]) =>
      providerClient.updateProviderSettings(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: providersKeys.all }),
  });
}

// useCreateCustomProvider creates a tenant custom OpenAI-compatible provider.
export function useCreateCustomProvider() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: Parameters<typeof providerClient.createCustomProvider>[0]) =>
      providerClient.createCustomProvider(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: providersKeys.all }),
  });
}

// useUpdateCustomProvider edits a custom provider (ref id immutable).
export function useUpdateCustomProvider() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: Parameters<typeof providerClient.updateCustomProvider>[0]) =>
      providerClient.updateCustomProvider(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: providersKeys.all }),
  });
}

// useDeleteCustomProvider deletes a custom provider; deletion is blocked
// while workers still reference it (guard error lists them).
export function useDeleteCustomProvider() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: Parameters<typeof providerClient.deleteCustomProvider>[0]) =>
      providerClient.deleteCustomProvider(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: providersKeys.all }),
  });
}

// useSetProviderToken auto-writes the tenant secret under the standard
// name (built-ins / custom CUSTOM_<REF>_API_KEY) — write-only.
export function useSetProviderToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: Parameters<typeof providerClient.setProviderToken>[0]) =>
      providerClient.setProviderToken(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: providersKeys.all }),
  });
}

// useClearProviderToken removes the stored token (env fallback applies).
export function useClearProviderToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: Parameters<typeof providerClient.clearProviderToken>[0]) =>
      providerClient.clearProviderToken(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: providersKeys.all }),
  });
}

// useProviderModels surfaces the sourcing state for one provider (probed +
// manual merged, deduped; hidden entries stay listed with visible=false so
// they can be re-checked; degraded=true = probe failure, non-fatal).
export function useProviderModels(providerId: string, enabled = true) {
  return useQuery({
    queryKey: providersKeys.models(providerId),
    queryFn: async () => {
      const res = await providerClient.listProviderModels({ providerId });
      return {
        models: (res.models ?? []) as ProviderModel[],
        degraded: res.degraded,
        enabled: res.enabled,
      };
    },
    enabled: enabled && providerId !== "",
  });
}

// useProviderModelsForPicker projects the providers-service sourcing view
// into the picker's OpenCodeModel shape (ADR-0004 three-tier contract).
// This is the NATIVE adapter's model tier: the vendored catalog ⊕ probed ⊕
// manual models from Settings → Adapters, NOT the opencode-CLI discovery
// (whose provider namespace the native adapter does not share). Entries
// keep providerId = the provider id and id = the bare model id, so
// catalogModelMatches resolves 3-segment refs by segments; modelRef
// mirrors the legacy 2-segment "providerId/id" shape.
export function useProviderModelsForPicker(providerId: string, enabled = true) {
  const q = useProviderModels(providerId, enabled);
  const models = q.data?.models;
  const projected = useMemo(() => {
    if (!models) return undefined;
    return models.map((m) => {
      const context = Number(m.context) || 0;
      const maxOutput = Number(m.maxOutput) || 0;
      return new OpenCodeModel({
        id: m.id,
        providerId,
        name: m.id,
        modelRef: `${providerId}/${m.id}`,
        family: providerId,
        status: "active",
        limits:
          context > 0 || maxOutput > 0
            ? { context: BigInt(context), output: BigInt(maxOutput) }
            : undefined,
        capabilities: m.reasoning ? { reasoning: true } : undefined,
        variants: m.reasoning ? ["low", "medium", "high"] : [],
      });
    });
  }, [models, providerId]);
  return {
    models: projected,
    isLoading: q.isLoading,
    error: q.error,
    degraded: q.data?.degraded ?? false,
  };
}
