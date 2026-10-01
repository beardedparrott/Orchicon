// MCP server management hooks (ADR-0008): the tenant-facing MCP surface
// behind Settings → Adapters → MCP. CRUD over tenant-scoped MCP server
// entries (stdio + streamable HTTP), the curated registry catalog with
// one-click prefill, explicit-only auto-install (dry-run in CI), and
// write-only credentials via the tenant secrets store (never returned).
//
// A definition is OWNER-SCOPED (project XOR conversation) and there is no
// separate selection: the owner column IS the selection. Every mutation
// invalidates the shared keys so every consumer auto-refreshes on save.
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";

import { mcpClient } from "@/api/clients";
import type { MCPServer, MCPCatalogEntry } from "@/api/gen/orchicon/api/v1/mcp_server_pb";

export const mcpKeys = {
  all: ["mcp-servers"] as const,
  catalog: ["mcp-catalog"] as const,
  runtimes: ["mcp-runtimes"] as const,
  // Owner-scoped scopes: the definitions a project / conversation owns.
  ownerProject: (projectId: string) => ["mcp-servers", "owner-project", projectId] as const,
  ownerConversation: (conversationId: string) =>
    ["mcp-servers", "owner-conversation", conversationId] as const,
};

// useMCPServerList lists MCP definitions. The SCOPE FILTER IS THE
// INHERITANCE QUERY: the owner column IS the selection, so narrowing to a
// project returns exactly the servers that project contributes — there is
// no separate "inherited" RPC and no client-side union. With no scope the
// list is the whole tenant (kept for callers that have no scope); every
// screen passes a scope.
// `enabled` lets a caller with NO scope (the worker-version placement, whose
// entries are inline specs in the caller's array) switch the query OFF rather
// than fire an unscoped one. An unscoped ListMCPServers is the whole-tenant
// list the epic removes, so firing it and ignoring the result is not acceptable.
export function useMCPServerList(
  scope?: { projectId?: string; conversationId?: string },
  opts?: { enabled?: boolean },
) {
  const key = scope?.projectId
    ? mcpKeys.ownerProject(scope.projectId)
    : scope?.conversationId
      ? mcpKeys.ownerConversation(scope.conversationId)
      : mcpKeys.all;
  return useQuery({
    queryKey: key,
    enabled: opts?.enabled ?? true,
    queryFn: async () => {
      const res = await mcpClient.listMCPServers({
        projectId: scope?.projectId ?? "",
        conversationId: scope?.conversationId ?? "",
      });
      return (res.servers ?? []) as MCPServer[];
    },
  });
}

export function useMCPCatalog() {
  return useQuery({
    queryKey: mcpKeys.catalog,
    queryFn: async () => {
      const res = await mcpClient.listMCPCatalog({});
      return (res.entries ?? []) as MCPCatalogEntry[];
    },
  });
}

export function useMCPRuntimes() {
  return useQuery({
    queryKey: mcpKeys.runtimes,
    queryFn: async () => {
      const res = await mcpClient.detectMCPRuntimes({});
      return (res.available ?? {}) as Record<string, boolean>;
    },
    refetchInterval: 30_000,
  });
}

export function useCreateMCPServer() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: Parameters<typeof mcpClient.createMCPServer>[0]) =>
      mcpClient.createMCPServer(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: mcpKeys.all }),
  });
}

export function useUpdateMCPServer() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: Parameters<typeof mcpClient.updateMCPServer>[0]) =>
      mcpClient.updateMCPServer(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: mcpKeys.all }),
  });
}

export function useDeleteMCPServer() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: Parameters<typeof mcpClient.deleteMCPServer>[0]) =>
      mcpClient.deleteMCPServer(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: mcpKeys.all }),
  });
}

// useInstallMCPServer runs an explicit auto-install (or a dry-run report
// when dryRun=true — never an implicit network install at session time).
export function useInstallMCPServer() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: {
      id: string;
      dryRun?: boolean;
    }) => mcpClient.installMCPRuntime({ id: req.id, dryRun: req.dryRun ?? false }),
    onSuccess: () => qc.invalidateQueries({ queryKey: mcpKeys.all }),
  });
}

// usePrefillMCPCatalogEntry returns a create-request prefilled from a
// catalog entry (one-click add). No server is created — the UI opens the
// form with the prefilled values.
export function usePrefillMCPCatalogEntry() {
  return useMutation({
    mutationFn: async (req: Parameters<typeof mcpClient.prefillMCPCatalogEntry>[0]) =>
      mcpClient.prefillMCPCatalogEntry(req),
  });
}

// useSetMCPServerSecret writes a credential to the tenant secrets store
// (provider-token pattern — never baked, never returned).
export function useSetMCPServerSecret() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: Parameters<typeof mcpClient.setMCPServerSecret>[0]) =>
      mcpClient.setMCPServerSecret(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: mcpKeys.all }),
  });
}

export function useClearMCPServerSecret() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: Parameters<typeof mcpClient.clearMCPServerSecret>[0]) =>
      mcpClient.clearMCPServerSecret(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: mcpKeys.all }),
  });
}
