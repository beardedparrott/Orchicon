import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { askOrchiconClient } from "@/api/clients";
import type { Conversation } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import type { ChatMessage } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import type { AgentConfig } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import type { SessionPermissionGrant } from "@/api/gen/orchicon/api/v1/ask_orchicon_service_pb";
import { ConversationMode } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";

export const askKeys = {
  conversations: ["ask", "conversations"] as const,
  conversation: (id: string) => ["ask", "conversation", id] as const,
  messages: (id: string) => ["ask", "messages", id] as const,
  config: ["ask", "config"] as const,
  // grants are the conversation's ACTIVE session grants (in-memory on the
  // plane, revoked from here).
  grants: (id: string) => ["ask", "grants", id] as const,
};

export function useListConversations(opts?: { refetchInterval?: number | false }) {
  return useQuery({
    queryKey: askKeys.conversations,
    queryFn: async () => {
      const res = await askOrchiconClient.listConversations({ pageSize: 50 });
      return (res.conversations ?? []) as Conversation[];
    },
    // The caller polls while any conversation has a running turn so running
    // state (sidebar indicators + Stop affordances) stays fresh across tabs
    // and devices; false = no polling.
    refetchInterval: opts?.refetchInterval ?? false,
  });
}

export function useGetConversation(id: string) {
  return useQuery({
    queryKey: askKeys.conversation(id),
    queryFn: async () => {
      const res = await askOrchiconClient.getConversation({ id });
      return res.conversation as Conversation | undefined;
    },
    enabled: !!id,
  });
}

export function useCreateConversation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (opts: {
      modelRef?: string;
      initialMessage?: string;
      mode?: ConversationMode;
      // projectId places the conversation in a project from birth — the
      // per-project "new conversation" button in the sidebar. Empty (the
      // default) creates an unassigned conversation, which is what every other
      // caller wants.
      projectId?: string;
    }) => {
      const res = await askOrchiconClient.createConversation({
        modelRef: opts.modelRef ?? "",
        initialMessage: opts.initialMessage ?? "",
        mode: opts.mode ?? ConversationMode.BRAINSTORM,
        projectId: opts.projectId ?? "",
      });
      return res.conversation as Conversation | undefined;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: askKeys.conversations });
    },
  });
}

// useSetConversationProject moves a conversation into a project, or unassigns it
// with an empty projectId. It is what the sidebar's project folders accept on a
// drop and what its per-project "new conversation" button sets at create time —
// the same rpc the TUI's /project calls, so the two clients cannot disagree
// about what "belongs to a project" means.
export function useSetConversationProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (opts: { id: string; projectId: string }) => {
      const res = await askOrchiconClient.setConversationProject({
        id: opts.id,
        projectId: opts.projectId,
      });
      return res.conversation as Conversation | undefined;
    },
    onSuccess: (_data, variables) => {
      qc.invalidateQueries({ queryKey: askKeys.conversations });
      qc.invalidateQueries({ queryKey: askKeys.conversation(variables.id) });
    },
  });
}

export function useDeleteConversation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await askOrchiconClient.deleteConversation({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: askKeys.conversations });
    },
  });
}

export function useUpdateConversationTitle() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (opts: { id: string; title: string }) => {
      const res = await askOrchiconClient.updateConversationTitle({ id: opts.id, title: opts.title });
      return res.conversation as Conversation | undefined;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: askKeys.conversations });
    },
  });
}

export function useCompactConversation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (conversationId: string) => {
      // A synchronous RPC: compaction is one bounded operation, so there is no
      // stream and no turn. It may run a summarize model call server-side, which
      // is why the caller must not treat this as an instant write.
      return await askOrchiconClient.compactConversation({ conversationId, reason: "manual" });
    },
    onSuccess: (_res, conversationId) => {
      // The server rewrote the conversation's history (the summary is a new
      // message), so both the transcript and the conversation list are stale.
      qc.invalidateQueries({ queryKey: askKeys.messages(conversationId) });
      qc.invalidateQueries({ queryKey: askKeys.conversations });
    },
  });
}

export function useListMessages(conversationId: string, opts?: { refetchInterval?: number | false }) {
  return useQuery({
    queryKey: askKeys.messages(conversationId),
    queryFn: async () => {
      const res = await askOrchiconClient.listMessages({ conversationId, pageSize: 200 });
      return (res.messages ?? []).reverse() as ChatMessage[];
    },
    enabled: !!conversationId,
    // Poll while a turn is pending so the detached collector's persisted
    // reply (or error) appears without a manual refresh.
    refetchInterval: opts?.refetchInterval ?? false,
  });
}

export function useAbortConversationTurn() {
  return useMutation({
    mutationFn: async (conversationId: string) => {
      await askOrchiconClient.abortConversationTurn({ conversationId });
    },
  });
}

export function useGetAgentConfig() {
  return useQuery({
    queryKey: askKeys.config,
    queryFn: async () => {
      const res = await askOrchiconClient.getAgentConfig({});
      return res.config;
    },
  });
}

export function useUpdateAgentConfig() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (config: AgentConfig) => {
      const res = await askOrchiconClient.updateAgentConfig({ config });
      return res.config;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: askKeys.config });
    },
  });
}

export function useSetConversationMode() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (opts: { id: string; mode: ConversationMode }) => {
      const res = await askOrchiconClient.setConversationMode({
        id: opts.id,
        mode: opts.mode,
      });
      return res.conversation as Conversation | undefined;
    },
    onSuccess: (_data, variables) => {
      qc.invalidateQueries({ queryKey: askKeys.conversations });
      qc.invalidateQueries({ queryKey: askKeys.conversation(variables.id) });
    },
  });
}

// useSetConversationModel retargets an OPEN conversation's model (ADR-0004
// picker → SetConversationModel). The change applies from the NEXT message.
export function useSetConversationModel() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (opts: { id: string; modelRef: string }) => {
      const res = await askOrchiconClient.setConversationModel({
        id: opts.id,
        modelRef: opts.modelRef,
      });
      return res.conversation as Conversation | undefined;
    },
    onSuccess: (_data, variables) => {
      // The conversation row carries the new model_ref, and the composer's stat
      // strip reads it back off that row — so both keys must refresh.
      qc.invalidateQueries({ queryKey: askKeys.conversations });
      qc.invalidateQueries({ queryKey: askKeys.conversation(variables.id) });
    },
  });
}

// --- session grants -------------------------------------------------------
//
// The plane's grant store is the single source of truth: these hooks read it and
// mutate it, and NEVER patch a local copy. A revoke takes effect on the next
// tool call (the execution guard reads the same store), which is why the
// response's refreshed list is written straight into the cache.

export function useListPermissionGrants(
  conversationId: string,
  opts?: { refetchInterval?: number | false },
) {
  return useQuery({
    queryKey: askKeys.grants(conversationId),
    queryFn: async () => {
      const res = await askOrchiconClient.listPermissionGrants({ conversationId });
      return (res.grants ?? []) as SessionPermissionGrant[];
    },
    enabled: !!conversationId,
    // Polled only while the grants panel is open: the list is small and
    // changes only when the operator decides something.
    refetchInterval: opts?.refetchInterval ?? false,
  });
}

export function useRevokePermissionGrant(conversationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (directory: string) =>
      await askOrchiconClient.revokePermissionGrant({ conversationId, directory }),
    onSuccess: (res) => {
      qc.setQueryData(
        askKeys.grants(conversationId),
        (res.grants ?? []) as SessionPermissionGrant[],
      );
      qc.invalidateQueries({ queryKey: askKeys.grants(conversationId) });
    },
  });
}
