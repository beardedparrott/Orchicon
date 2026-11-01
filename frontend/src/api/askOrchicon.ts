import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { askOrchiconClient } from "@/api/clients";
import type { Conversation } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import type { ChatMessage } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import type { AgentConfig } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import type { PermissionAsk, SessionPermissionGrant } from "@/api/gen/orchicon/api/v1/ask_orchicon_service_pb";
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
    // AND IT KEEPS POLLING WHILE THE TAB IS NOT FOCUSED, like the other Ask pollers (see the note on
    // usePendingAsks): the running markers and Stop affordances matter most on a tab the operator has
    // stepped away from and come back to, and a window-focus refetch now catches that up regardless.
    refetchIntervalInBackground: (opts?.refetchInterval ?? false) !== false,
    refetchOnWindowFocus: true,
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

// useSetConversationSkillFiles replaces the conversation's skill_files path
// list (SetConversationSkillFiles; an empty list clears it) — the
// conversation-level half of the skills feature, the exact mirror of
// useSetConversationProject. Rendered by the same contextfiles.RenderManifest
// as the project's, union-ed with it.
export function useSetConversationSkillFiles(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (files: string[]) => {
      const res = await askOrchiconClient.setConversationSkillFiles({ id, files });
      return res.conversation as Conversation | undefined;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: askKeys.conversations });
      qc.invalidateQueries({ queryKey: askKeys.conversation(id) });
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

/**
 * MessageDeleteMode mirrors the proto enum, re-exported so a caller names the intent rather than a number.
 *
 * ONLY removes exactly the named message (the GUI's "Undo this message"); AND_FOLLOWING removes it and
 * everything after it ("Revert to here"). The distinction is the operator's own wording, and the server
 * enforces it — see the RPC's doc comment.
 */
export const MessageDeleteMode = {
  ONLY: 1,
  AND_FOLLOWING: 2,
} as const;

/**
 * useDeleteConversationMessage removes one message, or that message and everything after it.
 *
 * THE SERVER ALSO RESETS THE CONVERSATION'S MODEL SESSION, and that is the half that matters: turns are
 * CONTINUED on a stored session id, so removing rows alone would take the message off the screen while
 * leaving it in what the model reasons about. The response reports whether a session was reset, so the
 * caller can say so.
 *
 * A turn in flight is REFUSED by the server (the reply is being written as it streams), so the caller must
 * not offer this while a turn is running.
 */
export function useDeleteConversationMessage(conversationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { messageId: string; mode: number }) => {
      return await askOrchiconClient.deleteConversationMessage({
        conversationId,
        messageId: input.messageId,
        mode: input.mode as never,
      });
    },
    onSuccess: () => {
      // The transcript shrank and the conversation's session_id changed, so both are stale.
      qc.invalidateQueries({ queryKey: askKeys.messages(conversationId) });
      qc.invalidateQueries({ queryKey: askKeys.conversations });
      qc.invalidateQueries({ queryKey: askKeys.conversation(conversationId) });
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

/**
 * usePendingAsks asks the SERVER which asks are still open for a conversation.
 *
 * WHY A QUERY AND NOT ONLY THE STREAM ARM. An ask arrives on the turn stream, so a
 * client learns of one only if it is WATCHING that turn at that instant. The GUI
 * usually is — which is why it appeared to work — but "usually" is not a guarantee:
 * a dropped socket, a re-dial that has not re-attached, a turn this tab did not
 * start, or a reload all leave a card with no path to the screen while the server
 * keeps the turn parked on it.
 *
 * The transcript ledger cannot cover this: it records the OUTCOME of a decision
 * (`permission.<verdict>`), so it can settle a card but can never reveal an open
 * one — there is nothing in it for an ask nobody has answered yet. That is the gap
 * this closes, and it is the same one that made the TUI show a stall while the GUI
 * displayed a waiting card.
 *
 * Results are folded in with applyAskChunk, which dedupes by ask id, so calling this
 * alongside the stream is free: a card delivered both ways is drawn once.
 */
export function usePendingAsks(conversationId: string, opts?: { refetchInterval?: number | false }) {
  return useQuery({
    queryKey: ["ask", "pendingAsks", conversationId] as const,
    queryFn: async () => {
      const res = await askOrchiconClient.listPendingAsks({ conversationId });
      return res.asks as PermissionAsk[];
    },
    enabled: Boolean(conversationId),
    // POLLED WHILE A TURN RUNS, and this is the fix for "every time a card was passed, I had to hit
    // refresh on the browser for it to appear".
    //
    // This query is the DISCOVERY half of the card path, and its own doc says it exists so "neither
    // client depends on having caught an event". It did not do that: it had NO refetch interval, so it
    // read ONCE at mount and never again. An ask raised by a turn already in flight therefore reached
    // this client only if the live socket delivered it — and when that arm missed (a dropped or
    // half-open socket, a watcher that was not subscribed at the moment the ask was raised, a turn
    // started in the other client), NOTHING re-asked, so the card was invisible until a page reload
    // re-ran the query from scratch. That is exactly the reported symptom, and it is the same defect
    // the transcript poll exists to prevent on the message side.
    //
    // The cadence follows the transcript poll, so the two cannot disagree about how fresh the pane is:
    // both are gated on the turn union (this client's slot OR the server's own turn_in_flight), which
    // is what covers a turn this client never streamed.
    refetchInterval: opts?.refetchInterval ?? false,
    // AND IT KEEPS POLLING WHILE THE TAB IS NOT FOCUSED — the half of this bug that the interval alone
    // did not fix.
    //
    // TanStack defaults `refetchIntervalInBackground` to FALSE, which PAUSES a poller whenever its
    // window is not focused; `refetchOnWindowFocus` is also false globally (main.tsx). So a card raised
    // while the operator was looking at another window — or recording, which is how this was reported —
    // had NO path to the screen: the live socket only pushes to a subscribed client, the discovery read
    // was paused, and nothing caught up on return. The reload they were pressing was the only thing that
    // re-ran the query, which is exactly "I had to hit refresh to see the card".
    //
    // A CARD IS AN OPERATOR-BLOCKING WAIT: the turn is parked until someone answers, so background
    // polling here is not churn — reaching the person who is NOT looking at the tab is the entire point.
    // `refetchOnWindowFocus` is opted back IN for the same reason, so returning to the tab repaints
    // immediately rather than waiting out the next interval.
    refetchIntervalInBackground: (opts?.refetchInterval ?? false) !== false,
    refetchOnWindowFocus: true,
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
    // KEEPS POLLING WHILE UNFOCUSED, for the same reason as the pending-ask read above: this is the
    // pane's only other path to a running turn's output, and a paused poller plus
    // refetchOnWindowFocus:false meant a hidden tab showed nothing new until it was reloaded.
    refetchIntervalInBackground: (opts?.refetchInterval ?? false) !== false,
    refetchOnWindowFocus: true,
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

// useSetConversationFullsend turns FULLSEND on or off for one conversation.
//
// THE STATE IS NOT HELD HERE. The mutation's job is to tell the server; the control renders
// from Conversation.fullsend, which the invalidation below refreshes. A local optimistic flag
// would show "on" for a write that failed, and this is the one control where that lie matters:
// the operator would proceed believing the gate is down while it is still up.
//
// It sends a VALUE rather than a toggle, for the same reason the TUI does — a retry after a
// dropped response must not invert the mode.
export function useSetConversationFullsend() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (opts: { id: string; enabled: boolean }) => {
      const res = await askOrchiconClient.setConversationFullsend({
        id: opts.id,
        enabled: opts.enabled,
      });
      return res.conversation as Conversation | undefined;
    },
    onSuccess: (_data, variables) => {
      // Both keys: the composer reads the control's state off the conversation row, and the
      // sidebar/rail list carries the same field.
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
