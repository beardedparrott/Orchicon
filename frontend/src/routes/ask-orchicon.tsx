import { createRoute } from "@tanstack/react-router";
import { useState, useCallback, useRef, useEffect, useMemo } from "react";
import {
  Plus,
  Trash2,
  Paperclip,
  Mic,
  Square,
  RefreshCw,
  Settings2,
  Brain,
  Pencil,
  FolderPlus,
  ChevronRight,
  GripVertical,
} from "lucide-react";

import { Route as rootRoute } from "@/routes/__root";

import { useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { ModeToggle } from "@/components/ui/mode-toggle";
import { cn } from "@/lib/utils";
import {
  useListConversations,
  useCreateConversation,
  useDeleteConversation,
  useUpdateConversationTitle,
  useListMessages,
  useGetConversation,
  useAbortConversationTurn,
  useSetConversationMode,
  askKeys,
} from "@/api/askOrchicon";
import { useGetSettings } from "@/api/settings";
import { askOrchiconClient } from "@/api/clients";
import { useToast, useToastStore } from "@/components/ui/toast";
import { ConversationMode } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import type {
  ChatMessage,
  Conversation,
} from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import { AttachmentInput } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import {
  UserBubble,
  AssistantBubble,
  ErrorBubble,
  ReasoningBubble,
  ChatScrollContainer,
} from "@/components/chat";
import { useCategoryPreferences, getItemsForCategory } from "@/lib/category-store";
import { CreateCategoryDialog } from "@/components/CreateCategoryDialog";
import {
  DndContext,
  DragOverlay,
  PointerSensor,
  useSensor,
  useSensors,
  type DragEndEvent,
  type DragOverEvent,
} from "@dnd-kit/core";
import { useDroppable } from "@dnd-kit/core";
import { useDraggable } from "@dnd-kit/core";
import { SortableContext, verticalListSortingStrategy } from "@dnd-kit/sortable";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/ask-orchicon",
  component: AskOrchiconPage,
});

// --- draft persistence constants ---
const DRAFT_STORAGE_KEY_PREFIX = "orchicon:draft:";

// --- clipboard helper (strict superset of inline navigator.clipboard?.writeText patterns) ---
function copyTextToClipboard(text: string): void {
  navigator.clipboard?.writeText(text).catch(() => {});
}

// --- streaming item types (mirrors execution ChatItem for Ask Orchicon) ---

type StreamItem =
  | { kind: "user"; text: string; at: number; key: string }
  | { kind: "text"; text: string; at: number; key: string; phase?: string }
  | { kind: "reasoning"; text: string; at: number; key: string; phase?: string }
  | { kind: "error"; text: string; at: number; key: string };

// Phase-group streaming items so interleaved reasoning/text chunks
// coalesce into one growing reasoning bubble and one growing text bubble.
function groupStreamItems(items: StreamItem[]): StreamItem[] {
  const out: StreamItem[] = [];
  let textBuf = "";
  let textAt = 0;
  let textKey = "";
  let textPhase = "";
  let reasoningBuf = "";
  let reasoningAt = 0;
  let reasoningKey = "";
  let reasoningPhase = "";

  const flushText = () => {
    if (!textBuf) return;
    out.push({
      kind: "text",
      text: textBuf,
      at: textAt,
      key: textKey,
      phase: textPhase,
    });
    textBuf = "";
  };
  const flushReasoning = () => {
    if (!reasoningBuf) return;
    out.push({
      kind: "reasoning",
      text: reasoningBuf,
      at: reasoningAt,
      key: reasoningKey,
      phase: reasoningPhase,
    });
    reasoningBuf = "";
  };

  for (const item of items) {
    if (item.kind === "text") {
      if (textPhase && item.phase !== textPhase) flushText();
      if (!textBuf) {
        textKey = item.key;
        textPhase = item.phase ?? "";
      }
      textBuf += item.text;
      textAt = item.at;
    } else if (item.kind === "reasoning") {
      if (reasoningPhase && item.phase !== reasoningPhase) flushReasoning();
      if (!reasoningBuf) {
        reasoningKey = item.key;
        reasoningPhase = item.phase ?? "";
      }
      reasoningBuf += item.text;
      reasoningAt = item.at;
    } else {
      flushText();
      flushReasoning();
      out.push(item);
    }
  }
  flushText();
  flushReasoning();
  return out;
}

// Per-conversation streaming state. Each conversation keeps its own
// in-flight turn so navigating away and back never drops the Stop button
// or the growing reasoning bubble, and isStreaming stays true for the
// conversation that is actually still processing (which also blocks a
// duplicate send instead of hitting the server's "already one processing").
interface ConvStream {
  isStreaming: boolean;
  isThinking: boolean;
  // reconnecting marks an ACKED turn whose ChatStream socket dropped — the
  // detached collector keeps running server-side, so the slot stays attached
  // (Stop + interject remain) and completion is resolved via the poll.
  reconnecting: boolean;
  optimisticUserMsg: string | null;
  pendingReplyId: string | null;
  // sentText is the message text captured at send time, held in-memory so the
  // completion effect can copy it to the clipboard if the turn is ACKED but
  // the reply later fails. Cleared on completion (success or failure) — it is
  // not a persisted draft, so it never resurrects into the composer.
  sentText: string | null;
  items: StreamItem[];
}
const EMPTY_STREAM: ConvStream = {
  isStreaming: false,
  isThinking: false,
  reconnecting: false,
  optimisticUserMsg: null,
  pendingReplyId: null,
  sentText: null,
  items: [],
};

// The server's last-resort fallback when no Ask Orchicon model is configured
// (conversation model_ref empty AND tenant default empty). Mirrors chat.go's
// hardcoded fallback — the free model is rate-limited and is the #1 cause of
// "Ask Orchicon is stuck" (a silent provider 429 looks like a stall).
const FALLBACK_ASK_MODEL = "opencode/deepseek-v4-flash-free";

function AskOrchiconPage() {
  const [activeConvId, setActiveConvId] = useState<string | null>(null);
  const toast = useToast();
  // Mode state — defaults to BRAINSTORM; synced from activeConv when available.
  const [localMode, setLocalMode] = useState<ConversationMode>(
    ConversationMode.BRAINSTORM,
  );

  // Live streaming state keyed by conversation id.
  const [streams, setStreams] = useState<Record<string, ConvStream>>({});

  // Per-conversation dispatch generation. Each send/interject bumps it; a
  // stream's fail() only owns the slot while its generation is current. This
  // stops a superseded (older) turn's socket-drop from tearing down the
  // interject turn that replaced it — the classic stale-closure hazard once
  // two streams can overlap for a conversation.
  const dispatchGenRef = useRef<Record<string, number>>({});

  // Conversation-list poll cadence: 3s while ANY conversation has a running
  // turn (so running indicators + Stop affordances stay live across tabs and
  // devices), false when nothing is running — no idle network churn. The
  // effect that updates it lives below with the conversations query.
  const [listPollMs, setListPollMs] = useState<number | false>(false);

  // Conversation rename state
  const [renamingConvId, setRenamingConvId] = useState<string | null>(null);
  const [renameValue, setRenameValue] = useState("");
  const renameInputRef = useRef<HTMLInputElement>(null);

  // Folder dialog state
  const [folderDialogOpen, setFolderDialogOpen] = useState(false);
  const [renamingFolderId, setRenamingFolderId] = useState<string | null>(null);
  const [folderRenameValue, setFolderRenameValue] = useState("");
  const folderRenameInputRef = useRef<HTMLInputElement>(null);

  // Drag-and-drop state
  const [activeDragId, setActiveDragId] = useState<string | null>(null);
  const [overFolderId, setOverFolderId] = useState<string | null>(null);

  // Category preferences for conversations (no auto-seed)
  const convPrefs = useCategoryPreferences("conversations", { noSeed: true });

  // Update one conversation's stream slot with a functional updater so
  // async chunk handlers never read stale state (append via the previous
  // value, not a captured `streams`).
  const setStream = useCallback(
    (convId: string, updater: (prev: ConvStream) => ConvStream) => {
      setStreams((prev) => ({
        ...prev,
        [convId]: updater(prev[convId] ?? EMPTY_STREAM),
      }));
    },
    [],
  );

  // The ACTIVE conversation's derived streaming state.
  const activeStream = activeConvId ? streams[activeConvId] : undefined;
  const isStreaming = activeStream?.isStreaming ?? false;
  const isThinking = activeStream?.isThinking ?? false;
  const reconnecting = activeStream?.reconnecting ?? false;
  const optimisticUserMsg = activeStream?.optimisticUserMsg ?? null;
  const pendingReplyId = activeStream?.pendingReplyId ?? null;
  const streamItems = activeStream?.items ?? [];

  const { data: conversations, isLoading: convsLoading } =
    useListConversations({
      // Poll while ANY conversation has a running turn so the sidebar's
      // running indicators + Stop buttons stay live across tabs and devices
      // (a turn started elsewhere appears within a few seconds), and stop
      // polling once everything settles — no idle network churn.
      refetchInterval: listPollMs,
    });
  const { data: messages, isLoading: msgsLoading } = useListMessages(
    activeConvId ?? "",
    { refetchInterval: isStreaming ? 2000 : false },
  );
  const { data: activeConv } = useGetConversation(activeConvId ?? "");
  const { data: settings } = useGetSettings();

  // Keep the conversation-list poll live while any conversation is running
  // and stop it once everything settles (see listPollMs above). The condition
  // is the UNION of local streaming (a turn this page started — the earliest
  // signal, since the server flag only becomes visible once we poll) and the
  // server-reported turn_in_flight (a turn started elsewhere / another tab,
  // discovered on the next poll). Polling from the moment a local turn starts
  // means the sidebar's running dot + Stop button appear immediately, not just
  // after a refresh.
  const anyStreaming = useMemo(
    () => Object.values(streams).some((s) => s.isStreaming),
    [streams],
  );
  useEffect(() => {
    const serverRunning = conversations?.some((c) => c.turnInFlight) ?? false;
    setListPollMs(anyStreaming || serverRunning ? 3000 : false);
  }, [conversations, anyStreaming]);
  const createConv = useCreateConversation();
  const deleteConv = useDeleteConversation();
  const updateTitle = useUpdateConversationTitle();
  const abortTurn = useAbortConversationTurn();
  const setMode = useSetConversationMode();
  const qc = useQueryClient();

  // The effective model answering this conversation: the conversation's own
  // model_ref wins, then the tenant default, then the server's free fallback.
  // Surfacing this directly answers "which model is answering?" — the most
  // common cause of a stuck turn is a silently-fallen-back free model.
  const effectiveModel = useMemo(
    () =>
      activeConv?.modelRef ||
      settings?.defaultAskOrchiconModel ||
      FALLBACK_ASK_MODEL,
    [activeConv?.modelRef, settings?.defaultAskOrchiconModel],
  );
  const isUsingFallbackModel =
    !activeConv?.modelRef && !settings?.defaultAskOrchiconModel;

  // Sync local mode from active conversation when it loads.
  useEffect(() => {
    if (activeConv?.mode && (activeConv.mode as number) !== ConversationMode.UNSPECIFIED) {
      setLocalMode(activeConv.mode);
    }
  }, [activeConv?.mode]);

  // The reply (or error) is persisted under the acked assistant message id.
  // When it appears via polling, the turn is over — clear the ACTIVE
  // conversation's stream slot (other conversations keep their own state,
  // so a turn running while you browse elsewhere stays intact). When the
  // acked message came back as an ERROR (reply failed after ack), the sent
  // text (held in-memory on the slot) is copied to the clipboard and written
  // back to sessionStorage so it is restorable — never lost on a failed reply.
  useEffect(() => {
    if (!activeConvId || !isStreaming || !pendingReplyId || !messages) return;
    const acked = messages.find((m) => m.id === pendingReplyId);
    if (!acked) return;
    if (acked.metadata?.error) {
      // Reply failed after the message was acked. The composer already
      // cleared on ack (the message is persisted in history), but put the
      // sent text on the clipboard and write it back to sessionStorage so it
      // is restorable — the user never loses what they typed.
      const sent = streams[activeConvId]?.sentText;
      if (sent) {
        copyTextToClipboard(sent);
        try {
          sessionStorage.setItem(`${DRAFT_STORAGE_KEY_PREFIX}${activeConvId}`, sent);
        } catch {
          // sessionStorage unavailable — ignore.
        }
      }
      toast.error("The reply failed. Your message was saved — retry below.", {
        title: "Reply failed",
      });
    }
    setStream(activeConvId, (prev) => ({
      ...prev,
      isStreaming: false,
      isThinking: false,
      reconnecting: false,
      optimisticUserMsg: null,
      pendingReplyId: null,
      sentText: null,
      items: [],
    }));
    qc.invalidateQueries({ queryKey: askKeys.conversations });
  }, [messages, isStreaming, pendingReplyId, activeConvId, streams, setStream, qc, toast]);

  // Re-attach a running turn after a refresh / from another tab or device.
  // The in-memory stream slot is gone (or never existed), but the server-side
  // turn registry is authoritative: when it reports a turn in flight for the
  // active conversation and the local slot is idle, restore the slot (Stop
  // button + thinking indicator + completion poll) keyed to the server's
  // pending assistant message id. A live local slot is never overwritten —
  // server state only fills gaps, so a locally-started turn keeps its own
  // stream until it completes.
  useEffect(() => {
    if (!activeConvId) return;
    const server = conversations?.find((c) => c.id === activeConvId);
    const serverRunning =
      server?.turnInFlight ?? activeConv?.turnInFlight ?? false;
    if (!serverRunning) return;
    if (streams[activeConvId]?.isStreaming) return;
    setStream(activeConvId, (prev) => {
      if (prev.isStreaming) return prev;
      return {
        ...prev,
        isStreaming: true,
        isThinking: true,
        reconnecting: true,
        pendingReplyId:
          server?.pendingAssistantMessageId ||
          activeConv?.pendingAssistantMessageId ||
          prev.pendingReplyId,
        items: [],
      };
    });
  }, [activeConvId, activeConv, conversations, streams, setStream]);

  const handleNewChat = useCallback(() => {
    setActiveConvId(null);
  }, []);

  const handleDeleteConv = useCallback(
    async (id: string, e: React.MouseEvent) => {
      e.stopPropagation();
      try {
        await deleteConv.mutateAsync(id);
        // Drop the deleted conversation's stream slot (its turn is gone).
        setStreams((prev) => {
          if (!(id in prev)) return prev;
          const next = { ...prev };
          delete next[id];
          return next;
        });
        if (activeConvId === id) {
          setActiveConvId(null);
        }
      } catch {
        toast.error("Failed to delete conversation", { title: "Error" });
      }
    },
    [deleteConv, activeConvId, toast],
  );

  // Start renaming a conversation
  const startRenameConv = useCallback(
    (convId: string, currentTitle: string, e: React.MouseEvent) => {
      e.stopPropagation();
      setRenamingConvId(convId);
      setRenameValue(currentTitle || "");
      requestAnimationFrame(() => renameInputRef.current?.select());
    },
    [],
  );

  // Save conversation rename
  const saveRenameConv = useCallback(
    async (convId: string) => {
      const trimmed = renameValue.trim();
      if (trimmed && trimmed !== conversations?.find((c) => c.id === convId)?.title) {
        try {
          await updateTitle.mutateAsync({ id: convId, title: trimmed });
        } catch {
          toast.error("Failed to rename conversation", { title: "Error" });
        }
      }
      setRenamingConvId(null);
      setRenameValue("");
    },
    [renameValue, conversations, updateTitle, toast],
  );

  // Cancel conversation rename
  const cancelRenameConv = useCallback(() => {
    setRenamingConvId(null);
    setRenameValue("");
  }, []);

  // Start renaming a folder
  const startRenameFolder = useCallback(
    (folderId: string, currentName: string) => {
      setRenamingFolderId(folderId);
      setFolderRenameValue(currentName);
      requestAnimationFrame(() => folderRenameInputRef.current?.select());
    },
    [],
  );

  // Save folder rename
  const saveRenameFolder = useCallback(
    (folderId: string) => {
      const trimmed = folderRenameValue.trim();
      if (trimmed) {
        convPrefs.renameCategory(folderId, trimmed);
      }
      setRenamingFolderId(null);
      setFolderRenameValue("");
    },
    [folderRenameValue, convPrefs],
  );

  // Cancel folder rename
  const cancelRenameFolder = useCallback(() => {
    setRenamingFolderId(null);
    setFolderRenameValue("");
  }, []);

  // Stop a running turn on any conversation — the active conversation's Stop
  // button or a sidebar row's. Aborts the server-side turn (idempotent) and
  // drops the local stream slot so the UI recovers immediately; the
  // conversation-list refetch clears the running indicator once the server
  // finalizes the abort.
  const handleStopConversation = useCallback(
    async (convId: string) => {
      try {
        await abortTurn.mutateAsync(convId);
        setStream(convId, (prev) => ({
          ...prev,
          isStreaming: false,
          isThinking: false,
          reconnecting: false,
          optimisticUserMsg: null,
          pendingReplyId: null,
          sentText: null,
          items: [],
        }));
        qc.invalidateQueries({ queryKey: askKeys.conversations });
      } catch {
        toast.error("Failed to stop the reply", { title: "Error" });
      }
    },
    [abortTurn, setStream, toast, qc],
  );

  const handleStopStreaming = useCallback(() => {
    if (!activeConvId) return;
    return handleStopConversation(activeConvId);
  }, [activeConvId, handleStopConversation]);

  // Streaming helper — takes convId as a parameter so it is never stale.
  // Mutates the given conversation's OWN stream slot via functional
  // updaters, so a turn keeps running (and the UI keeps updating) even
  // while the user is browsing a different conversation. Returns `true`
  // when the stream observed TurnStarted (server acked/persisted the
  // user message); `false` on any pre-ack failure.
  const runStream = useCallback(
    async (
      convId: string,
      text: string,
      attachments: AttachmentInput[] | undefined,
      mode: "send" | "interject",
    ): Promise<boolean> => {
      const gen = (dispatchGenRef.current[convId] ?? 0) + 1;
      dispatchGenRef.current[convId] = gen;

      const stream =
        mode === "interject"
          ? askOrchiconClient.interjectConversationTurn({
              conversationId: convId,
              message: text,
              attachments: attachments ?? [],
            })
          : askOrchiconClient.chatStream({
              conversationId: convId,
              message: text,
              attachments: attachments ?? [],
            });

      // fail tears down the stream slot ONLY when the turn was never acked
      // (no pendingReplyId) AND this stream is still the current dispatch.
      // Once acked, the server-side collector runs on a request-independent
      // context — a dropped socket (network blip, server restart,
      // backgrounded tab) must NOT orphan the turn from the UI: the slot
      // stays streaming so the Stop button and the interject input remain,
      // and the existing ListMessages poll resolves completion when the
      // persisted reply/error appears. A stale generation (a newer interject
      // owns the slot) never touches the state.
      const fail = (err?: unknown) => {
        setStream(convId, (prev) => {
          if (dispatchGenRef.current[convId] !== gen) {
            return prev;
          }
          if (prev.pendingReplyId) {
            return { ...prev, isThinking: false, reconnecting: true };
          }
          return {
            ...prev,
            isStreaming: false,
            isThinking: false,
            reconnecting: false,
            optimisticUserMsg: null,
            items: [],
          };
        });
        if (err) {
          toast.error(String(err instanceof Error ? err.message : err), { title: "Chat error" });
        }
      };

      let acked: boolean = false;
      try {
        for await (const chunk of stream) {
          if (chunk.event.case === "turnStarted") {
            const assistantMessageId = chunk.event.value.assistantMessageId;
            setStream(convId, (prev) => ({
              ...prev,
              pendingReplyId: assistantMessageId,
              reconnecting: false,
            }));
            acked = true;
          } else if (chunk.event.case === "textChunk") {
            const content = chunk.event.value.content;
            if (content) {
              setStream(convId, (prev) => ({
                ...prev,
                items: [
                  ...prev.items,
                  {
                    kind: "text",
                    text: content,
                    at: Date.now(),
                    key: `st-${Date.now()}-${Math.random()}`,
                    phase: "p-0",
                  },
                ],
              }));
            }
          } else if (chunk.event.case === "reasoning") {
            const content = chunk.event.value.content;
            if (content) {
              setStream(convId, (prev) => ({
                ...prev,
                items: [
                  ...prev.items,
                  {
                    kind: "reasoning",
                    text: content,
                    at: Date.now(),
                    key: `sr-${Date.now()}-${Math.random()}`,
                    phase: "p-0",
                  },
                ],
              }));
            }
          } else if (chunk.event.case === "error") {
            toast.error(chunk.event.value.message);
            fail();
            return false;
          }
        }
        if (!acked) {
          fail();
        }
      } catch (err: unknown) {
        fail(err);
      }
      return acked;
    },
    [toast, setStream],
  );

  // A normal send: starts a fresh turn on the conversation.
  const sendStreaming = useCallback(
    async (convId: string, text: string, attachments?: AttachmentInput[]): Promise<boolean> => {
      setStream(convId, (prev) => ({
        ...prev,
        optimisticUserMsg: text,
        isStreaming: true,
        isThinking: true,
        reconnecting: false,
        pendingReplyId: null,
        sentText: text,
        items: [],
      }));
      return runStream(convId, text, attachments, "send");
    },
    [runStream, setStream],
  );

  // An interjection: sent while a turn is already streaming. The server
  // supersedes the in-flight turn (persisting its partial content as a plain
  // message) and answers this message on a fresh turn that acks a NEW
  // assistant message id. The old id's partial message appears via the poll
  // and must NOT clear the new turn — the poll effect keys on the current
  // pendingReplyId, which turnStarted now points at the new id.
  const interjectStreaming = useCallback(
    async (convId: string, text: string, attachments?: AttachmentInput[]): Promise<boolean> => {
      setStream(convId, (prev) => ({
        ...prev,
        optimisticUserMsg: text,
        isStreaming: true,
        isThinking: true,
        reconnecting: false,
        pendingReplyId: null,
        sentText: text,
        items: [],
      }));
      return runStream(convId, text, attachments, "interject");
    },
    [runStream, setStream],
  );

  const handleSendMessage = useCallback(
    async (text: string, attachments?: AttachmentInput[]): Promise<boolean> => {
      if (!text.trim() || !activeConvId) return false;
      if (isStreaming) {
        // Send while streaming = interject: interrupt the current reply and
        // redirect the model (the server aborts the session + supersedes the
        // turn), never the "another reply already processing" dead-end.
        const ok = await interjectStreaming(activeConvId, text, attachments);
        if (!ok) {
          copyTextToClipboard(text);
        }
        return ok;
      }
      const ok = await sendStreaming(activeConvId, text, attachments);
      if (!ok) {
        copyTextToClipboard(text);
      }
      return ok;
    },
    [activeConvId, isStreaming, sendStreaming, interjectStreaming],
  );

  const handleRetry = useCallback((): Promise<boolean> => {
    const lastUser = messages
      ?.slice()
      .reverse()
      .find((m) => m.role === "user");
    if (lastUser?.content) {
      return handleSendMessage(lastUser.content);
    }
    return Promise.resolve(false);
  }, [messages, handleSendMessage]);

  // Optimistic mode toggle — flips local state immediately, API in background.
  const handleModeChange = useCallback(
    (next: ConversationMode) => {
      const prev = localMode;
      setLocalMode(next);
      if (activeConvId) {
        setMode.mutate(
          { id: activeConvId, mode: next },
          {
            onError: () => {
              setLocalMode(prev);
              toast.error("Failed to change mode", { title: "Error" });
            },
          },
        );
      }
    },
    [localMode, activeConvId, setMode, toast],
  );

  // DnD sensors
  const dndSensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 5 } }),
  );

  // Handle drag start
  const handleDragStart = useCallback(
    (event: { active: { id: string | number } }) => {
      setActiveDragId(String(event.active.id));
      setOverFolderId(null);
    },
    [],
  );

  // Handle drag over (for visual feedback on folder drop targets)
  const handleDragOver = useCallback(
    (event: DragOverEvent) => {
      const { over } = event;
      if (over?.id) {
        setOverFolderId(String(over.id));
      } else {
        setOverFolderId(null);
      }
    },
    [],
  );

  // Handle drag end (assign conversation to folder)
  const handleDragEnd = useCallback(
    (event: DragEndEvent) => {
      const { active, over } = event;
      setActiveDragId(null);
      setOverFolderId(null);
      if (!over) return;
      const convId = String(active.id);
      const targetId = String(over.id);
      // Drop on "uncategorized" = remove assignment
      if (targetId === "__uncategorized__") {
        convPrefs.assignItem(convId, "");
      } else {
        // Drop on a folder
        const targetFolder = convPrefs.state.categories.find((c) => c.id === targetId);
        if (targetFolder) {
          convPrefs.assignItem(convId, targetId);
        }
      }
    },
    [convPrefs],
  );

  // Build categorized conversation groups
  const categorizedConversations = useMemo(() => {
    if (!conversations) return { categorized: new Map<string, string[]>(), uncategorized: [] as string[] };
    return getItemsForCategory(convPrefs.state, conversations.map((c) => c.id));
  }, [conversations, convPrefs.state]);

  // Map for quick lookup of conversations by id. Carries the server-reported
  // turn state so the sidebar can show which conversations are busy.
  const convById = useMemo(() => {
    if (!conversations) return new Map<string, Conversation>();
    return new Map(conversations.map((c) => [c.id, c]));
  }, [conversations]);

  // Group streaming items into coalesced bubbles.
  const groupedStream = useMemo(
    () => groupStreamItems(streamItems),
    [streamItems],
  );

  // Merge durable messages with streaming items for display.
  // Durable messages are the source of truth; streaming items fill in the gap.
  const displayMessages = useMemo(() => {
    if (!isStreaming || groupedStream.length === 0) return messages ?? [];
    // Build a simple merged list: durable messages + streaming items.
    return [...(messages ?? []), ...groupedStream] as ChatMessage[];
  }, [messages, isStreaming, groupedStream]);

  return (
    <div className="flex h-[calc(100vh-3.5rem)] gap-0">
      {/* Main chat area — centered column */}
      <div className="flex flex-1 flex-col min-w-0">
        {!activeConvId ? (
          /* --- Greeting state: centered vertical + horizontal --- */
          <div className="flex flex-1 items-center justify-center px-4">
            <div className="w-full max-w-2xl space-y-8">
              <div className="text-center space-y-4">
                <h1 className="text-4xl font-extrabold tracking-tight text-foreground md:text-5xl">
                  What would you like to create today?
                </h1>
                <p className="text-base text-muted-foreground max-w-lg mx-auto md:text-lg">
                  Ask Orchicon anything — create projects, manage work items,
                  brainstorm ideas, or get help with your codebase.
                </p>
              </div>
               <ChatInputField
                onSend={async (text, attachments) => {
                  // Create a conversation first, then send via the streaming
                  // helper directly (avoids the stale-closure on handleSendMessage).
                  try {
                    const conv = await createConv.mutateAsync({
                      mode: localMode,
                    });
                    if (conv?.id) {
                      setActiveConvId(conv.id);
                      const ok = await sendStreaming(conv.id, text, attachments);
                      if (!ok) {
                        copyTextToClipboard(text);
                      }
                      return ok;
                    }
                  } catch {
                    toast.error("Failed to create conversation", {
                      title: "Error",
                    });
                  }
                  return false;
                }}
                onStop={handleStopStreaming}
                isStreaming={isStreaming}
                placeholder="Ask Orchicon anything..."
                mode={localMode}
                onModeChange={handleModeChange}
              />
            </div>
          </div>
        ) : (
          /* --- Active conversation: scroll + pinned input --- */
          <div className="flex flex-1 flex-col min-h-0">
            {/* Chat header */}
            <div className="flex items-center justify-between border-b px-6 py-3 shrink-0">
              <div className="flex items-center gap-2 min-w-0">
                <h2 className="text-sm font-medium truncate">
                  {activeConv?.title || "Ask Orchicon"}
                </h2>
                {localMode === ConversationMode.BRAINSTORM ? (
                  <span className="shrink-0 inline-flex items-center gap-1 rounded-full bg-violet-100 px-2 py-0.5 text-[10px] font-medium text-violet-700 dark:bg-violet-900/30 dark:text-violet-300">
                    <Brain className="h-2.5 w-2.5" />
                    Brainstorm
                  </span>
                ) : (
                  <span className="shrink-0 inline-flex items-center gap-1 rounded-full bg-sky-100 px-2 py-0.5 text-[10px] font-medium text-sky-700 dark:bg-sky-900/30 dark:text-sky-300">
                    <Settings2 className="h-2.5 w-2.5" />
                    Orchicon
                  </span>
                )}
                {/* The model answering this conversation — surfaced so a
                    silent fallback to the free model is never invisible. */}
                <span
                  className={cn(
                    "shrink-0 max-w-[220px] truncate inline-flex items-center rounded-md px-2 py-0.5 font-mono text-[10px]",
                    isUsingFallbackModel
                      ? "bg-amber-100 text-amber-800 dark:bg-amber-900/30 dark:text-amber-300"
                      : "bg-muted text-muted-foreground",
                  )}
                  title={
                    isUsingFallbackModel
                      ? "No Ask Orchicon model is configured — using the free fallback model, which is rate-limited and can appear stuck."
                      : "The model answering this conversation"
                  }
                >
                  {effectiveModel}
                </span>
              </div>
              <Button variant="ghost" size="sm" onClick={handleNewChat}>
                <Plus className="h-4 w-4" />
              </Button>
            </div>

            {/* Fallback-model warning: no Ask Orchicon model is configured,
                so the server is answering with the free fallback model. That
                model is rate-limited and is the #1 cause of a stuck turn —
                surfacing it turns a confusing "Orchicon is thinking" into an
                actionable "your model config is missing". */}
            {isUsingFallbackModel && (
              <div className="flex items-start gap-2 border-b border-amber-300/40 bg-amber-50/60 px-6 py-2 text-xs text-amber-800 dark:border-amber-900/40 dark:bg-amber-950/20 dark:text-amber-300">
                <RefreshCw className="mt-0.5 h-3 w-3 shrink-0" />
                <span className="min-w-0 [overflow-wrap:anywhere]">
                  No Ask Orchicon model is configured — answering with the free
                  fallback model (<span className="font-mono">{effectiveModel}</span>),
                  which is rate-limited and can appear stuck. Set a model in
                  Settings → Default models.
                </span>
              </div>
            )}

            {/* Messages — auto-stick scroll */}
            <ChatScrollContainer
              items={[displayMessages, isThinking, optimisticUserMsg]}
            >
              <div className="space-y-4 px-6 py-6">
                {msgsLoading && (
                  <p className="text-center text-sm text-muted-foreground">
                    Loading messages...
                  </p>
                )}
                {!msgsLoading &&
                  displayMessages.length === 0 &&
                  !optimisticUserMsg && (
                    <div className="text-center text-sm text-muted-foreground py-8">
                      Start a conversation by typing a message below.
                    </div>
                  )}

                {/* Persisted messages from the server */}
                {messages?.map((msg) => (
                  <MessageBubble
                    key={msg.id}
                    message={msg}
                    onRetry={handleRetry}
                  />
                ))}

                {/* Optimistic user message — before streaming bubbles */}
                {optimisticUserMsg &&
                  !messages?.some(
                    (m) =>
                      m.content === optimisticUserMsg && m.role === "user",
                  ) && (
                    <UserBubble
                      text={optimisticUserMsg}
                      source="you"
                    />
                  )}

                {/* Live streaming items (text + reasoning chunks) */}
                {isStreaming &&
                  groupedStream.map((item) => {
                    switch (item.kind) {
                      case "text":
                        return (
                          <AssistantBubble
                            key={item.key}
                            text={item.text}
                          />
                        );
                      case "reasoning":
                        return (
                          <ReasoningBubble
                            key={item.key}
                            text={item.text}
                            streaming={isStreaming}
                          />
                        );
                      case "error":
                        return (
                          <ErrorBubble key={item.key} text={item.text} />
                        );
                      default:
                        return null;
                    }
                  })}

                {/* Thinking indicator — visible until any streaming content arrives */}
                {isThinking && groupedStream.length === 0 && (
                  <div className="flex justify-start">
                    <div className="max-w-[88%] rounded-2xl rounded-tl-sm border border-sky-300/30 bg-sky-50/20 px-4 py-3 dark:border-sky-950/40 dark:bg-sky-950/10">
                      <div className="flex items-center gap-2">
                        <span className="shrink-0 inline-block h-1.5 w-1.5 rounded-full bg-sky-500 animate-pulse" />
                        <span className="min-w-0 text-sm text-muted-foreground [overflow-wrap:anywhere]">
                          Orchicon is thinking
                          {isUsingFallbackModel && (
                            <span className="text-muted-foreground/70">
                              {" "}
                              ({effectiveModel} — free fallback, may be rate-limited)
                            </span>
                          )}
                          …
                        </span>
                      </div>
                    </div>
                  </div>
                )}

                {/* Reconnecting notice — the acked turn's socket dropped but
                    the detached collector keeps running server-side. The Stop
                    button + interject input stay available and the poll
                    resolves completion. */}
                {isStreaming && reconnecting && (
                  <div className="flex justify-center">
                    <p className="text-xs text-muted-foreground">
                      Connection lost — still working… You can interject or
                      stop this reply.
                    </p>
                  </div>
                )}

                <div className="h-4" />
              </div>
            </ChatScrollContainer>

            {/* Input — fixed at bottom */}
            <div className="shrink-0 border-t">
              <ChatInputField
                onSend={handleSendMessage}
                onStop={handleStopStreaming}
                isStreaming={isStreaming}
                placeholder="Ask Orchicon anything..."
                mode={localMode}
                onModeChange={handleModeChange}
                convId={activeConvId}
              />
            </div>
          </div>
        )}
      </div>

      {/* Right sidebar — conversations panel (w-80 per ADR-001) */}
      <aside className="hidden lg:flex w-80 shrink-0 flex-col border-l bg-card overflow-y-auto">
        <div className="flex items-center justify-between border-b px-4 py-3">
          <span className="text-sm font-medium">Conversations</span>
          <div className="flex items-center gap-1">
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setFolderDialogOpen(true)}
              title="New folder"
            >
              <FolderPlus className="h-4 w-4" />
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={handleNewChat}
            >
              <Plus className="h-4 w-4" />
            </Button>
          </div>
        </div>
        <div className="flex-1 overflow-y-auto p-2 space-y-1">
          {convsLoading && (
            <p className="text-xs text-center text-muted-foreground py-4">
              Loading...
            </p>
          )}
          {!convsLoading && (!conversations || conversations.length === 0) && (
            <p className="text-xs text-center text-muted-foreground py-4">
              No conversations yet
            </p>
          )}

          <DndContext
            sensors={dndSensors}
            onDragStart={handleDragStart}
            onDragOver={handleDragOver}
            onDragEnd={handleDragEnd}
          >
            <SortableContext
              items={conversations?.map((c) => c.id) ?? []}
              strategy={verticalListSortingStrategy}
            >
              {/* Folders */}
              {convPrefs.state.categories.map((category) => {
                const folderConvIds = categorizedConversations.categorized.get(category.id) ?? [];
                const isCollapsed = convPrefs.collapsed.has(category.id);
                const isOver = overFolderId === category.id;
                const isRenaming = renamingFolderId === category.id;

                return (
                  <FolderItem
                    key={category.id}
                    id={category.id}
                    name={category.name}
                    isCollapsed={isCollapsed}
                    isOver={isOver}
                    isRenaming={isRenaming}
                    renameValue={folderRenameValue}
                    renameInputRef={folderRenameInputRef}
                    onToggle={() => convPrefs.toggleCollapsed(category.id)}
                    onStartRename={() => startRenameFolder(category.id, category.name)}
                    onSaveRename={() => saveRenameFolder(category.id)}
                    onCancelRename={cancelRenameFolder}
                    onRenameChange={setFolderRenameValue}
                    onDelete={() => convPrefs.deleteCategory(category.id)}
                    convIds={folderConvIds}
                    convById={convById}
                    activeConvId={activeConvId}
                    renamingConvId={renamingConvId}
                    convRenameValue={renameValue}
                    convRenameInputRef={renameInputRef}
                    onSelectConv={setActiveConvId}
                    onStartRenameConv={startRenameConv}
                    onSaveRenameConv={saveRenameConv}
                    onCancelRenameConv={cancelRenameConv}
                    onRenameConvChange={setRenameValue}
                    onDeleteConv={handleDeleteConv}
                    onStopConv={handleStopConversation}
                    activeDragId={activeDragId}
                  />
                );
              })}

              {/* Uncategorized section — drop target for removing assignments */}
              <UncategorizedDropZone
                id="__uncategorized__"
                convIds={categorizedConversations.uncategorized}
                convById={convById}
                activeConvId={activeConvId}
                renamingConvId={renamingConvId}
                renameValue={renameValue}
                renameInputRef={renameInputRef}
                onSelectConv={setActiveConvId}
                onStartRenameConv={startRenameConv}
                onSaveRenameConv={saveRenameConv}
                onCancelRenameConv={cancelRenameConv}
                onRenameConvChange={setRenameValue}
                onDeleteConv={handleDeleteConv}
                onStopConv={handleStopConversation}
                activeDragId={activeDragId}
                isOver={overFolderId === "__uncategorized__"}
                hasFolders={convPrefs.state.categories.length > 0}
              />
            </SortableContext>
            <DragOverlay dropAnimation={null}>
              {activeDragId ? (
                <div className="rounded-md bg-background border shadow-md px-3 py-2 text-sm text-foreground max-w-[200px] truncate">
                  {convById.get(activeDragId)?.title || "New conversation"}
                </div>
              ) : null}
            </DragOverlay>
          </DndContext>
        </div>
      </aside>

      {/* Create Category Dialog */}
      <CreateCategoryDialog
        open={folderDialogOpen}
        onOpenChange={setFolderDialogOpen}
        onCreate={(name) => {
          convPrefs.createCategory(name);
          setFolderDialogOpen(false);
        }}
        existingNames={convPrefs.state.categories.map((c) => c.name)}
      />
    </div>
  );
}

// --- MessageBubble: renders a single persisted ChatMessage ----------

function MessageBubble({
  message,
  onRetry,
}: {
  message: ChatMessage;
  onRetry?: () => void;
}) {
  const isUser = message.role === "user";
  const isError = !!message.metadata?.error;

  if (isUser) {
    return <UserBubble text={message.content} source="you" />;
  }

  if (isError) {
    const errModel = message.metadata?.modelRef;
    return (
      <div className="flex justify-start">
        <div className="max-w-[88%] rounded-2xl rounded-tl-sm border border-destructive/50 bg-destructive/10 px-4 py-3">
          <p className="text-sm text-destructive mb-1">
            {message.metadata?.error}
          </p>
          {/* The model the failed turn dispatched on — a stalled Ask Orchicon
              turn is almost always a model/provider problem (rate limit,
              quota, unavailable model), and naming it turns a mystery into a
              fixable config check. */}
          {errModel && (
            <p className="text-xs text-muted-foreground mt-1 [overflow-wrap:anywhere]">
              Model: <span className="font-mono">{errModel}</span> — if this
              repeats, check Settings → Default models.
            </p>
          )}
          {onRetry && (
            <Button variant="outline" size="sm" onClick={onRetry} className="mt-2">
              <RefreshCw className="h-3.5 w-3.5 mr-1" />
              Retry
            </Button>
          )}
        </div>
      </div>
    );
  }

  // Assistant message — check for reasoning in metadata
  const reasoning = ("reasoning" in message ? (message as { reasoning?: string[] }).reasoning : undefined) as string[] | undefined;
  const hasReasoning = Array.isArray(reasoning) && reasoning.length > 0;

  return (
    <>
      {hasReasoning && (
        <ReasoningBubble text={reasoning!.join("\n")} />
      )}
      <AssistantBubble
        text={message.content}
        label="Orchicon"
      />
    </>
  );
}

// --- ChatInputField: auto-resizing textarea with attach/voice/send ---

function ChatInputField({
  onSend,
  onStop,
  isStreaming,
  placeholder = "Ask Orchicon anything...",
  mode = ConversationMode.BRAINSTORM,
  onModeChange,
  convId,
}: {
  onSend: (text: string, attachments?: AttachmentInput[]) => Promise<boolean>;
  onStop: () => void;
  isStreaming: boolean;
  placeholder?: string;
  mode?: ConversationMode;
  onModeChange?: (mode: ConversationMode) => void;
  convId?: string | null;
}) {
  // The input stays ENABLED while streaming: sending mid-reply is the
  // interject path (interrupt + redirect), not a rejected "already
  // processing" send. Only the Stop button is offered alongside Send.
  const [text, setText] = useState("");
  const [attachments, setAttachments] = useState<AttachmentInput[]>([]);
  const [sending, setSending] = useState(false);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // Restore draft from sessionStorage when conversation changes.
  useEffect(() => {
    if (!convId) {
      setText("");
      return;
    }
    try {
      const key = `${DRAFT_STORAGE_KEY_PREFIX}${convId}`;
      const saved = sessionStorage.getItem(key);
      if (saved) {
        setText(saved);
      }
    } catch {
      // sessionStorage unavailable — ignore.
    }
  }, [convId]);

  // Persist draft to sessionStorage as the user types.
  const handleTextChange = useCallback(
    (e: React.ChangeEvent<HTMLTextAreaElement>) => {
      const next = e.target.value;
      setText(next);
      if (!convId) return;
      try {
        const key = `${DRAFT_STORAGE_KEY_PREFIX}${convId}`;
        if (next) {
          sessionStorage.setItem(key, next);
        } else {
          sessionStorage.removeItem(key);
        }
      } catch {
        // sessionStorage unavailable — ignore.
      }
      e.target.style.height = "auto";
      e.target.style.height = `${Math.min(e.target.scrollHeight, 192)}px`;
    },
    [convId],
  );

  const handleSubmit = useCallback(async () => {
    if (sending) return;
    if (text.trim() || attachments.length > 0) {
      setSending(true);
      try {
        const ok = await onSend(
          text.trim(),
          attachments.length > 0 ? attachments : undefined,
        );
        if (ok) {
          setText("");
          setAttachments([]);
          if (inputRef.current) {
            inputRef.current.style.height = "auto";
          }
          if (convId) {
            try {
              sessionStorage.removeItem(`${DRAFT_STORAGE_KEY_PREFIX}${convId}`);
            } catch {
              // ignore
            }
          }
          // The composer clears on ack (the message is persisted server-side)
          // and the sessionStorage draft is cleared too, so a successful send
          // never resurrects the text. The sent text is held in-memory on the
          // stream slot (sentText) and written back to sessionStorage only if
          // the reply later fails — restorable only when it actually failed.
        }
        // on false: text stays so user can fix & retry.
      } finally {
        setSending(false);
      }
    }
  }, [text, attachments, onSend, sending, convId]);

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        handleSubmit();
      }
    },
    [handleSubmit],
  );

  const handleFileSelect = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      const file = e.target.files?.[0];
      if (!file) return;
      const reader = new FileReader();
      reader.onload = () => {
        const input = new AttachmentInput();
        input.name = file.name;
        input.mimeType = file.type;
        input.data = new Uint8Array(reader.result as ArrayBuffer);
        setAttachments((prev) => [...prev, input]);
      };
      reader.readAsArrayBuffer(file);
      e.target.value = "";
    },
    [],
  );

  const [isRecording, setIsRecording] = useState(false);
  const mediaRecorderRef = useRef<MediaRecorder | null>(null);
  const audioChunksRef = useRef<Blob[]>([]);

  const handleVoiceInput = useCallback(async () => {
    const pushToast = useToastStore.getState().push;

    if (
      "webkitSpeechRecognition" in window ||
      "SpeechRecognition" in window
    ) {
      const SpeechRecognitionCtor =
        window.SpeechRecognition ?? window.webkitSpeechRecognition;
      if (!SpeechRecognitionCtor) return;
      const recognition = new SpeechRecognitionCtor();
      recognition.lang = "en-US";
      recognition.interimResults = false;
      recognition.onresult = (event: SpeechRecognitionEvent) => {
        const transcript = event.results[0][0].transcript;
        setText((prev) => (prev ? prev + " " : "") + transcript);
      };
      recognition.onerror = () => {
        pushToast({
          kind: "error",
          message:
            "Voice input failed. Check your microphone permissions.",
        });
      };
      recognition.start();
      return;
    }

    if (!navigator.mediaDevices?.getUserMedia) {
      pushToast({
        kind: "info",
        message:
          "Voice input requires a browser with SpeechRecognition (Chrome/Edge) or media recording support.",
      });
      return;
    }

    try {
      const stream = await navigator.mediaDevices.getUserMedia({
        audio: true,
      });
      const recorder = new MediaRecorder(stream);
      mediaRecorderRef.current = recorder;
      audioChunksRef.current = [];
      setIsRecording(true);

      recorder.ondataavailable = (e) => {
        if (e.data.size > 0) audioChunksRef.current.push(e.data);
      };

      recorder.onstop = () => {
        setIsRecording(false);
        stream.getTracks().forEach((t) => t.stop());
        const blob = new Blob(audioChunksRef.current, {
          type: "audio/webm",
        });
        const reader = new FileReader();
        reader.onload = () => {
          const input = new AttachmentInput();
          input.name = "voice_input.webm";
          input.mimeType = "audio/webm";
          input.data = new Uint8Array(reader.result as ArrayBuffer);
          setAttachments((prev) => [...prev, input]);
          pushToast({
            kind: "success",
            message:
              "Audio recorded and attached. Send your message to have Orchicon process it.",
          });
        };
        reader.readAsArrayBuffer(blob);
      };

      recorder.onerror = () => {
        setIsRecording(false);
        stream.getTracks().forEach((t) => t.stop());
        pushToast({ kind: "error", message: "Recording failed." });
      };

      recorder.start();
      setTimeout(() => {
        if (recorder.state === "recording") recorder.stop();
      }, 10000);
    } catch {
      pushToast({
        kind: "error",
        message: "Microphone access denied or unavailable.",
      });
    }
  }, []);

  const handleStopRecording = useCallback(() => {
    if (mediaRecorderRef.current?.state === "recording") {
      mediaRecorderRef.current.stop();
    }
  }, []);

  return (
    <div className="p-4">
      {attachments.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-2">
          {attachments.map((a, i) => (
            <span
              key={i}
              className="inline-flex items-center gap-1 rounded-md bg-muted px-2 py-1 text-xs"
            >
              <Paperclip className="h-3 w-3" />
              {a.name}
              <button
                onClick={() =>
                  setAttachments((prev) =>
                    prev.filter((_, j) => j !== i),
                  )
                }
                className="ml-1 text-muted-foreground hover:text-foreground"
              >
                ×
              </button>
            </span>
          ))}
        </div>
      )}
      <div className="rounded-xl border bg-background focus-within:ring-2 focus-within:ring-ring focus-within:ring-offset-2 overflow-hidden">
        {/* Textarea area */}
        <div className="flex items-end gap-1 px-3 pt-3 pb-1">
          <button
            onClick={() => fileInputRef.current?.click()}
            className="shrink-0 rounded p-1 text-muted-foreground hover:text-foreground hover:bg-accent"
            title="Attach file"
          >
            <Paperclip className="h-4 w-4" />
          </button>
          <textarea
            ref={inputRef}
            value={text}
            onChange={handleTextChange}
            onKeyDown={handleKeyDown}
            placeholder={placeholder}
            rows={1}
            className="flex-1 resize-none bg-transparent px-1 py-0 text-sm leading-6 outline-none placeholder:text-muted-foreground min-h-[24px] max-h-[192px]"
          />
          <button
            onClick={isRecording ? handleStopRecording : handleVoiceInput}
            className={cn(
              "shrink-0 rounded p-1",
              isRecording
                ? "text-destructive hover:bg-destructive/10 animate-pulse"
                : "text-muted-foreground hover:text-foreground hover:bg-accent",
            )}
            title={isRecording ? "Stop recording" : "Voice input"}
          >
            <Mic className="h-4 w-4" />
          </button>
        </div>
        {/* Bottom toolbar — send/stop on left, mode dropdown on the right */}
        <div className="flex items-center justify-between border-t border-border/40 px-3 py-2.5">
          <div className="flex items-center gap-1.5">
            {isStreaming ? (
              <>
                <Button
                  onClick={handleSubmit}
                  disabled={!text.trim() && attachments.length === 0}
                  size="sm"
                  title="Send interrupts the current reply and redirects the model"
                >
                  Send
                </Button>
                <Button onClick={onStop} variant="destructive" size="sm">
                  <Square className="h-3.5 w-3.5 mr-1" />
                  Stop
                </Button>
              </>
            ) : (
              <Button
                onClick={handleSubmit}
                disabled={!text.trim() && attachments.length === 0}
                size="sm"
              >
                Send
              </Button>
            )}
          </div>
          <div className="flex items-center gap-2">
            {onModeChange && (
              <ModeToggle
                mode={mode}
                onModeChange={onModeChange}
              />
            )}
          </div>
        </div>
      </div>
      <input
        ref={fileInputRef}
        type="file"
        className="hidden"
        onChange={handleFileSelect}
      />
    </div>
  );
}

// --- Conversation sidebar components ---

interface ConversationItemProps {
  convId: string;
  title: string;
  lastMessagePreview?: string;
  isActive: boolean;
  isRenaming: boolean;
  // isRunning + onStop surface a running turn on this conversation (server
  // turn_in_flight): a pulsing dot + an always-visible Stop button, so a turn
  // is stoppable from the sidebar even when the conversation isn't the active
  // one (e.g. after a refresh, or a turn started in another tab/device).
  isRunning?: boolean;
  onStop?: () => void;
  renameValue: string;
  renameInputRef: React.MutableRefObject<HTMLInputElement | null>;
  onSelect: () => void;
  onStartRename: (e: React.MouseEvent) => void;
  onSaveRename: () => void;
  onCancelRename: () => void;
  onRenameChange: (value: string) => void;
  onDelete: (e: React.MouseEvent) => void;
  isDragging?: boolean;
}

function ConversationItem({
  convId,
  title,
  lastMessagePreview,
  isActive,
  isRenaming,
  isRunning = false,
  onStop,
  renameValue,
  renameInputRef,
  onSelect,
  onStartRename,
  onSaveRename,
  onCancelRename,
  onRenameChange,
  onDelete,
  isDragging,
}: ConversationItemProps) {
  const { attributes, listeners, setNodeRef, isDragging: isDndDragging } =
    useDraggable({ id: convId });

  return (
    <div
      ref={setNodeRef}
      className={cn(
        "rounded-md transition-colors group",
        isDndDragging && "z-10 opacity-50",
        isDragging && "opacity-50",
      )}
    >
      <button
        onClick={onSelect}
        className={cn(
          "w-full text-left rounded-md px-3 py-2 text-sm transition-colors",
          isActive
            ? "bg-accent text-accent-foreground"
            : "text-muted-foreground hover:bg-accent/50 hover:text-accent-foreground",
        )}
      >
        <div className="flex items-start gap-2">
          {/* Drag handle */}
          <span
            {...attributes}
            {...listeners}
            className="shrink-0 mt-0.5 cursor-grab active:cursor-grabbing opacity-0 group-hover:opacity-100 text-muted-foreground"
          >
            <GripVertical className="h-3 w-3" />
          </span>
          <div className="flex-1 min-w-0">
            {isRenaming ? (
              <input
                ref={renameInputRef}
                value={renameValue}
                onChange={(e) => onRenameChange(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") onSaveRename();
                  if (e.key === "Escape") onCancelRename();
                }}
                onBlur={onSaveRename}
                onClick={(e) => e.stopPropagation()}
                className="w-full bg-background border rounded px-1 py-0.5 text-sm outline-none focus:ring-1 focus:ring-ring"
              />
            ) : (
              <div className="flex items-center gap-1.5 min-w-0">
                {isRunning && (
                  <span
                    className="shrink-0 relative flex h-1.5 w-1.5"
                    title="Reply in progress"
                  >
                    <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-sky-400 opacity-75" />
                    <span className="relative inline-flex rounded-full h-1.5 w-1.5 bg-sky-500" />
                  </span>
                )}
                <span className="truncate block flex-1 min-w-0">
                  {title || "New conversation"}
                </span>
                {isRunning && onStop && (
                  <span
                    role="button"
                    tabIndex={0}
                    onClick={(e) => {
                      e.stopPropagation();
                      onStop();
                    }}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" || e.key === " ") {
                        e.preventDefault();
                        e.stopPropagation();
                        onStop();
                      }
                    }}
                    className="shrink-0 cursor-pointer text-destructive hover:text-destructive-foreground"
                    title="Stop this reply"
                  >
                    <Square className="h-3 w-3 fill-current" />
                  </span>
                )}
              </div>
            )}
            {lastMessagePreview && (
              <p className="mt-0.5 text-xs text-muted-foreground truncate">
                {lastMessagePreview}
              </p>
            )}
          </div>
          {!isRenaming && (
            <span className="shrink-0 flex items-center gap-0.5 opacity-0 group-hover:opacity-100">
              <button
                onClick={onStartRename}
                className="text-muted-foreground hover:text-foreground"
                title="Rename"
              >
                <Pencil className="h-3 w-3" />
              </button>
              <button
                onClick={onDelete}
                className="text-muted-foreground hover:text-destructive"
                title="Delete"
              >
                <Trash2 className="h-3 w-3" />
              </button>
            </span>
          )}
        </div>
      </button>
    </div>
  );
}

interface FolderItemProps {
  id: string;
  name: string;
  isCollapsed: boolean;
  isOver: boolean;
  isRenaming: boolean;
  renameValue: string;
  renameInputRef: React.MutableRefObject<HTMLInputElement | null>;
  onToggle: () => void;
  onStartRename: () => void;
  onSaveRename: () => void;
  onCancelRename: () => void;
  onRenameChange: (value: string) => void;
  onDelete: () => void;
  convIds: string[];
  convById: Map<string, Conversation>;
  activeConvId: string | null;
  renamingConvId: string | null;
  convRenameValue: string;
  convRenameInputRef: React.MutableRefObject<HTMLInputElement | null>;
  onSelectConv: (id: string) => void;
  onStartRenameConv: (id: string, title: string, e: React.MouseEvent) => void;
  onSaveRenameConv: (id: string) => void;
  onCancelRenameConv: () => void;
  onRenameConvChange: (value: string) => void;
  onDeleteConv: (id: string, e: React.MouseEvent) => void;
  onStopConv: (id: string) => void;
  activeDragId: string | null;
}

function FolderItem({
  id,
  name,
  isCollapsed,
  isOver,
  isRenaming,
  renameValue,
  renameInputRef,
  onToggle,
  onStartRename,
  onSaveRename,
  onCancelRename,
  onRenameChange,
  onDelete,
  convIds,
  convById,
  activeConvId,
  renamingConvId,
  convRenameValue,
  convRenameInputRef,
  onSelectConv,
  onStartRenameConv,
  onSaveRenameConv,
  onCancelRenameConv,
  onRenameConvChange,
  onDeleteConv,
  onStopConv,
  activeDragId,
}: FolderItemProps) {
  const { setNodeRef } = useDroppable({ id });

  return (
    <div
      ref={setNodeRef}
      className={cn(
        "rounded-md transition-colors",
        isOver && "bg-accent/50 ring-1 ring-ring",
      )}
    >
      {/* Folder header */}
      <div className="flex items-center gap-1 px-2 py-1.5 group">
        <button
          onClick={onToggle}
          className="shrink-0 p-0.5 text-muted-foreground hover:text-foreground"
        >
          <ChevronRight
            className={cn(
              "h-3 w-3 transition-transform",
              !isCollapsed && "rotate-90",
            )}
          />
        </button>
        {isRenaming ? (
          <input
            ref={renameInputRef}
            value={renameValue}
            onChange={(e) => onRenameChange(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") onSaveRename();
              if (e.key === "Escape") onCancelRename();
            }}
            onBlur={onSaveRename}
            className="flex-1 bg-background border rounded px-1 py-0.5 text-xs outline-none focus:ring-1 focus:ring-ring"
          />
        ) : (
          <span className="flex-1 text-xs font-medium truncate">{name}</span>
        )}
        {!isRenaming && (
          <span className="shrink-0 flex items-center gap-0.5 opacity-0 group-hover:opacity-100">
            <button
              onClick={onStartRename}
              className="text-muted-foreground hover:text-foreground"
              title="Rename folder"
            >
              <Pencil className="h-3 w-3" />
            </button>
            <button
              onClick={onDelete}
              className="text-muted-foreground hover:text-destructive"
              title="Delete folder"
            >
              <Trash2 className="h-3 w-3" />
            </button>
          </span>
        )}
      </div>

      {/* Folder contents — conversations inside the folder */}
      {!isCollapsed && convIds.length > 0 && (
        <div className="pl-4 space-y-0.5">
          {convIds.map((convId) => {
            const conv = convById.get(convId);
            if (!conv) return null;
            return (
              <ConversationItem
                key={convId}
                convId={convId}
                title={conv.title}
                lastMessagePreview={conv.lastMessagePreview}
                isRunning={conv.turnInFlight ?? false}
                onStop={() => onStopConv(convId)}
                isActive={activeConvId === convId}
                isRenaming={renamingConvId === convId}
                renameValue={convRenameValue}
                renameInputRef={convRenameInputRef}
                onSelect={() => onSelectConv(convId)}
                onStartRename={(e) => onStartRenameConv(convId, conv.title, e)}
                onSaveRename={() => onSaveRenameConv(convId)}
                onCancelRename={onCancelRenameConv}
                onRenameChange={onRenameConvChange}
                onDelete={(e) => onDeleteConv(convId, e)}
                isDragging={activeDragId === convId}
              />
            );
          })}
        </div>
      )}
    </div>
  );
}

interface UncategorizedDropZoneProps {
  id: string;
  convIds: string[];
  convById: Map<string, Conversation>;
  activeConvId: string | null;
  renamingConvId: string | null;
  renameValue: string;
  renameInputRef: React.MutableRefObject<HTMLInputElement | null>;
  onSelectConv: (id: string) => void;
  onStartRenameConv: (id: string, title: string, e: React.MouseEvent) => void;
  onSaveRenameConv: (id: string) => void;
  onCancelRenameConv: () => void;
  onRenameConvChange: (value: string) => void;
  onDeleteConv: (id: string, e: React.MouseEvent) => void;
  onStopConv: (id: string) => void;
  activeDragId: string | null;
  isOver: boolean;
  hasFolders: boolean;
}

function UncategorizedDropZone({
  id,
  convIds,
  convById,
  activeConvId,
  renamingConvId,
  renameValue,
  renameInputRef,
  onSelectConv,
  onStartRenameConv,
  onSaveRenameConv,
  onCancelRenameConv,
  onRenameConvChange,
  onDeleteConv,
  onStopConv,
  activeDragId,
  isOver,
  hasFolders,
}: UncategorizedDropZoneProps) {
  const { setNodeRef } = useDroppable({ id });

  // Only show the "Uncategorized" label when there are folders (otherwise it's redundant)
  if (convIds.length === 0 && !hasFolders) return null;

  return (
    <div
      ref={setNodeRef}
      className={cn(
        "rounded-md transition-colors",
        isOver && "bg-accent/50 ring-1 ring-ring",
      )}
    >
      {hasFolders && (
        <div className="px-2 py-1.5">
          <span className="text-xs text-muted-foreground">Uncategorized</span>
        </div>
      )}
      {convIds.map((convId) => {
        const conv = convById.get(convId);
        if (!conv) return null;
        return (
          <ConversationItem
            key={convId}
            convId={convId}
            title={conv.title}
            lastMessagePreview={conv.lastMessagePreview}
            isRunning={conv.turnInFlight ?? false}
            onStop={() => onStopConv(convId)}
            isActive={activeConvId === convId}
            isRenaming={renamingConvId === convId}
            renameValue={renameValue}
            renameInputRef={renameInputRef}
            onSelect={() => onSelectConv(convId)}
            onStartRename={(e) => onStartRenameConv(convId, conv.title, e)}
            onSaveRename={() => onSaveRenameConv(convId)}
            onCancelRename={onCancelRenameConv}
            onRenameChange={onRenameConvChange}
            onDelete={(e) => onDeleteConv(convId, e)}
            isDragging={activeDragId === convId}
          />
        );
      })}
    </div>
  );
}

export default AskOrchiconPage;
