import { createRoute, useNavigate } from "@tanstack/react-router";
import { useState, useCallback, useRef, useEffect, useMemo } from "react";
import {
  Plus,
  Trash2,
  Paperclip,
  Mic,
  Square,
  RefreshCw,
  Brain,
  Pencil,
  FolderPlus,
  ChevronRight,
  GripVertical,
  MessageSquare,
  PanelRight,
  PanelRightClose,
  History,
  Sparkles,
} from "lucide-react";

import { Link } from "@tanstack/react-router";
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
  validateSearch: (search: Record<string, unknown>) => ({
    conversationId: (search.conversationId as string | undefined) ?? null,
  }),
  component: AskOrchiconPage,
});

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
  const search = Route.useSearch() as { conversationId: string | null };
  const navigate = useNavigate();
  const activeConvId = search.conversationId ?? null;
  const setActiveConvId = useCallback(
    (id: string | null) => {
      navigate({
        to: "/ask-orchicon",
        search: { conversationId: id ?? undefined } as never,
      });
    },
    [navigate],
  );
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

  // Per-conversation "a local runStream is actively iterating" flag. The
  // completion effect must NOT finalize (clear) a stream slot while the local
  // stream is still delivering chunks — otherwise it clears on every poll and
  // the restore effect re-arms it, making the Stop button flicker continuously
  // and unclickable. Set true while runStream's for-await runs, false after.
  const liveStreamRef = useRef<Record<string, boolean>>({});
  // Per-conversation sent text, mirrored out of the stream slot so the
  // completion effect can copy it on reply-failure without depending on
  // `streams` (which changes on every chunk and would re-run the effect).
  const sentTextRef = useRef<Record<string, string>>({});

  // A reply-failure restore signal: when the completion effect detects a turn
  // that was acked but whose reply errored, it sets this so the active
  // ChatInputField puts the sent text back in the box. token changes each time
  // so a repeated failure re-triggers the restore.
  const [restoreDraft, setRestoreDraft] = useState<{
    convId: string;
    text: string;
    token: number;
  } | null>(null);

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

  // Category preferences for conversations (seed into Software Development once)
  const convPrefs = useCategoryPreferences("conversations");
  const [panelCollapsed, setPanelCollapsed] = useState<boolean>(() => {
    try {
      return localStorage.getItem("orchicon_conversation_panel_collapsed") === "true";
    } catch {
      return false;
    }
  });
  const togglePanel = useCallback(() => {
    setPanelCollapsed((prev) => {
      const next = !prev;
      try {
        localStorage.setItem("orchicon_conversation_panel_collapsed", String(next));
      } catch {}
      return next;
    });
  }, []);

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
  // text is copied to the clipboard and the composer is signalled to put it
  // back in the box so the user never loses what they typed on a failed reply.
  //
  // This effect does NOT depend on `streams` (which changes on every streaming
  // chunk): clearing the slot on every chunk while the local stream is still
  // delivering text made the restore effect re-arm it, so the Stop button
  // flickered continuously and could never be clicked. The liveStreamRef guard
  // (a local runStream actively iterating) prevents finalizing a live turn; the
  // effect only finalizes once the local stream has ended, or for a restored
  // server turn with no local stream.
  useEffect(() => {
    if (!activeConvId || !isStreaming || !pendingReplyId || !messages) return;
    if (liveStreamRef.current[activeConvId]) return;
    const acked = messages.find((m) => m.id === pendingReplyId);
    if (!acked) return;
    if (acked.metadata?.error) {
      // Reply failed after the message was acked. The composer already cleared
      // on send (the message is persisted in history), but copy the sent text
      // to the clipboard AND signal the composer to put it back in the box so
      // the user never loses what they typed.
      const sent = sentTextRef.current[activeConvId] ?? "";
      if (sent) {
        copyTextToClipboard(sent);
        setRestoreDraft({
          convId: activeConvId,
          text: sent,
          token: Date.now(),
        });
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
    delete sentTextRef.current[activeConvId];
    qc.invalidateQueries({ queryKey: askKeys.conversations });
  }, [messages, isStreaming, pendingReplyId, activeConvId, setStream, qc, toast]);

  // Re-attach a running turn after a refresh / from another tab or device.
  // The in-memory stream slot is gone (or never existed), but the server-side
  // turn registry is authoritative: when it reports a turn in flight for the
  // active conversation and the local slot is idle, restore the slot (Stop
  // button + thinking indicator + completion poll) keyed to the server's
  // pending assistant message id. A live local slot is never overwritten —
  // server state only fills gaps, so a locally-started turn keeps its own
  // stream until it completes.
  //
  // The server's turn flag can LAG the persisted reply by up to a poll cycle
  // (the registry entry is removed before the reply row is written). Re-arming
  // a turn whose reply is already persisted would ping-pong with the completion
  // effect — clearing here, re-arming there — flickering the Stop button and
  // re-sticking the scroll on every toggle. The acked assistant message only
  // ever exists in messages once the reply is persisted (never as a placeholder
  // at ack time), so skip re-arming when it is present: the turn is genuinely
  // done and the stale flag will clear on the next conversations poll.
  useEffect(() => {
    if (!activeConvId) return;
    const server = conversations?.find((c) => c.id === activeConvId);
    const serverRunning =
      server?.turnInFlight ?? activeConv?.turnInFlight ?? false;
    if (!serverRunning) return;
    if (streams[activeConvId]?.isStreaming) return;
    const pendingId =
      server?.pendingAssistantMessageId ||
      activeConv?.pendingAssistantMessageId ||
      "";
    if (pendingId && messages?.some((m) => m.id === pendingId)) return;
    setStream(activeConvId, (prev) => {
      if (prev.isStreaming) return prev;
      return {
        ...prev,
        isStreaming: true,
        isThinking: true,
        reconnecting: true,
        pendingReplyId: pendingId || prev.pendingReplyId,
        items: [],
      };
    });
  }, [activeConvId, activeConv, conversations, streams, messages, setStream]);

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
        liveStreamRef.current[convId] = false;
        delete sentTextRef.current[convId];
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
      liveStreamRef.current[convId] = true;
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
      } finally {
        liveStreamRef.current[convId] = false;
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
      sentTextRef.current[convId] = text;
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
      sentTextRef.current[convId] = text;
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

  // Seed existing conversations into "Software Development" once on first load
  useEffect(() => {
    if (conversations && conversations.length > 0) {
      convPrefs.ensureSeeded(conversations.map((c) => c.id));
    }
  }, [conversations]);

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
                <div className="flex justify-center">
                  <span className="inline-flex items-center gap-2 glass-panel rounded-full px-3.5 py-1.5 text-xs uppercase tracking-wider text-cyan-300">
                    <span className="h-1.5 w-1.5 rounded-full bg-cyan-400 animate-pulse" />
                    Intelligent Orchestration
                  </span>
                </div>
                <h1 className="text-5xl font-extrabold tracking-tight sm:text-6xl">
                  <span className="text-foreground">Orchestrate </span>
                  <span className="bg-gradient-to-r from-cyan-400 via-sky-300 to-indigo-300 bg-clip-text text-transparent">with intention</span>
                </h1>
                <p className="text-lg text-slate-400 max-w-2xl mx-auto font-light leading-relaxed text-balance">
                  Ask Orchicon anything. Plan, execute, and govern with real-time clarity and thin control.
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
                placeholder="Ask Orchicon Anything..."
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
                <span className="shrink-0 inline-flex items-center gap-1 rounded-full bg-violet-100 px-2 py-0.5 text-[10px] font-medium text-violet-700 dark:bg-violet-900/30 dark:text-violet-300">
                  <Brain className="h-2.5 w-2.5" />
                  Brainstorm
                </span>
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
                placeholder="Ask Orchicon Anything..."
                mode={localMode}
                onModeChange={handleModeChange}
                convId={activeConvId}
                restoreDraft={restoreDraft}
              />
            </div>
          </div>
        )}
      </div>

      {/* Right sidebar — conversations panel (w-72 glass-panel, route-local per ADR-0.1) */}
      {!panelCollapsed ? (
        <aside className="hidden lg:flex w-72 glass-panel rounded-2xl flex-col overflow-hidden border border-white/10 shadow-2xl relative z-20 shrink-0 max-h-[calc(100vh-5.5rem)]">
          <div className="p-3.5 border-b border-white/10 flex items-center justify-between shrink-0">
            <div className="flex items-center space-x-2 text-slate-300">
              <MessageSquare className="w-4 h-4 text-cyan-400" />
              <span className="text-xs font-semibold uppercase tracking-wider">Conversations</span>
            </div>
            <div className="flex items-center space-x-1">
              <button
                onClick={() => setFolderDialogOpen(true)}
                className="p-1 text-slate-400 hover:text-white hover:bg-white/10 rounded-md transition"
                title="New folder"
                aria-label="New folder"
              >
                <FolderPlus className="w-4 h-4" />
              </button>
              <Link
                to="/ask-orchicon"
                search={{ conversationId: undefined } as never}
                onClick={(e: React.MouseEvent) => { e.preventDefault(); handleNewChat(); }}
                className="p-1 text-slate-400 hover:text-white hover:bg-white/10 rounded-md transition"
                title="New Chat"
                aria-label="New conversation"
              >
                <Plus className="w-4 h-4" />
              </Link>
              <button
                onClick={togglePanel}
                className="p-1 text-slate-400 hover:text-white hover:bg-white/10 rounded-md transition"
                title="Collapse Panel"
                aria-label="Collapse conversation panel"
              >
                <PanelRightClose className="w-4 h-4" />
              </button>
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
          <div className="p-2 border-t border-white/10 bg-slate-900/30 shrink-0">
            <button
              onClick={handleNewChat}
              className="w-full flex items-center justify-center space-x-2 py-1.5 px-3 rounded-xl bg-white/5 hover:bg-white/10 text-slate-300 hover:text-white text-xs font-medium transition border border-white/5"
            >
              <History className="w-3.5 h-3.5 text-slate-400" />
              <span>View All History</span>
            </button>
          </div>
        </aside>
      ) : (
        <button
          onClick={togglePanel}
          aria-label="Expand conversation panel"
          title="Expand Conversations"
          className="hidden lg:flex h-fit p-2 glass-panel rounded-xl text-slate-400 hover:text-white hover:bg-white/10 transition self-start"
        >
          <PanelRight className="h-4 w-4" />
        </button>
      )}

      {/* Create Category Dialog */}
      <CreateCategoryDialog
        open={folderDialogOpen}
        onOpenChange={setFolderDialogOpen}
        onCreate={(name, description) => {
          convPrefs.createCategory(name, description);
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
  restoreDraft,
}: {
  onSend: (text: string, attachments?: AttachmentInput[]) => Promise<boolean>;
  onStop: () => void;
  isStreaming: boolean;
  placeholder?: string;
  mode?: ConversationMode;
  onModeChange?: (mode: ConversationMode) => void;
  convId?: string | null;
  // When the parent detects a reply failure (a turn that was acked but whose
  // reply errored), it signals this with the sent text so the composer puts
  // it back in the box. Null/absent = nothing to restore.
  restoreDraft?: { convId: string; text: string; token: number } | null;
}) {
  // The input stays ENABLED while streaming: sending mid-reply is the
  // interject path (interrupt + redirect), not a rejected "already
  // processing" send. Only the Stop button is offered alongside Send.
  const [text, setText] = useState("");
  const [attachments, setAttachments] = useState<AttachmentInput[]>([]);
  const [sending, setSending] = useState(false);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // On a reply failure the parent signals the text to put back in the box.
  // No sessionStorage draft persistence — the box is cleared on send and the
  // text is only restored when a send actually fails, so it never lingers
  // across conversations or while a reply is in flight.
  useEffect(() => {
    if (restoreDraft && restoreDraft.convId === convId && restoreDraft.text) {
      setText(restoreDraft.text);
    }
  }, [restoreDraft, convId]);

  const handleTextChange = useCallback(
    (e: React.ChangeEvent<HTMLTextAreaElement>) => {
      setText(e.target.value);
      e.target.style.height = "auto";
      e.target.style.height = `${Math.min(e.target.scrollHeight, 192)}px`;
    },
    [],
  );

  // --- attachment constants (server is authoritative: 5 / 10MB / 20MB) ---
  const MAX_ATTACHMENTS = 5;
  const MAX_FILE_BYTES = 10 * 1024 * 1024;
  const MAX_TOTAL_BYTES = 20 * 1024 * 1024;
  const ACCEPTED_EXTS = new Set([
    "png","jpg","jpeg","gif","webp","svg","bmp","pdf","txt","md","mdx","json","csv","yaml","yml","html","css","js","ts","tsx","go","py","rs","java","sh","xml","log",
  ]);
  const getExt = (name: string) => name.split(".").pop()?.toLowerCase() ?? "";
  const isAllowedFile = (file: File): boolean => {
    const mime = (file.type || "").toLowerCase();
    const ext = getExt(file.name);
    if (mime.startsWith("image/")) return true;
    if (mime.startsWith("text/")) return true;
    if (mime.includes("json") || mime.includes("csv") || mime.includes("pdf") || mime.includes("xml") || mime.includes("yaml")) return true;
    if (ext && ACCEPTED_EXTS.has(ext)) return true;
    // empty mime but known extension — allow
    if (!mime && ext && ACCEPTED_EXTS.has(ext)) return true;
    return false;
  };

  const [pendingReads, setPendingReads] = useState(0);

  const handleSubmit = useCallback(async () => {
    if (sending) return;
    if (pendingReads > 0) {
      useToastStore.getState().push({ kind: "info", message: `Still loading ${pendingReads} file(s)...` });
      return;
    }
    const sentText = text.trim();
    const sentAttachments = attachments.length > 0 ? attachments : undefined;
    if (sentText || attachments.length > 0) {
      setSending(true);
      // Clear the box immediately on send (no lingering text while the model
      // responds); the sent text is captured for recovery on failure.
      setText("");
      setAttachments([]);
      if (inputRef.current) {
        inputRef.current.style.height = "auto";
      }
      try {
        const ok = await onSend(sentText, sentAttachments);
        if (!ok) {
          // Pre-ack failure: put the text back so the user can fix & retry.
          setText(sentText);
          setAttachments(sentAttachments ?? []);
        }
      } finally {
        setSending(false);
      }
    }
  }, [sending, text, attachments, onSend, pendingReads]);

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
      const files = e.target.files;
      if (!files || files.length === 0) return;
      const existingTotal = attachments.reduce((s, a) => s + (a.data?.length ?? 0), 0);
      let queuedCount = 0;
      let queuedBytes = 0;
      for (const file of Array.from(files)) {
        if (attachments.length + queuedCount >= MAX_ATTACHMENTS) {
          useToastStore.getState().push({ kind: "error", message: `Too many attachments (max ${MAX_ATTACHMENTS})` });
          break;
        }
        if (!isAllowedFile(file)) {
          useToastStore.getState().push({ kind: "error", message: `Unsupported file type: ${file.name}` });
          continue;
        }
        if (file.size > MAX_FILE_BYTES) {
          useToastStore.getState().push({ kind: "error", message: `File too large (max 10MB): ${file.name}` });
          continue;
        }
        if (existingTotal + queuedBytes + file.size > MAX_TOTAL_BYTES) {
          useToastStore.getState().push({ kind: "error", message: `Attachments too large (max 20MB total)` });
          continue;
        }
        queuedCount++;
        queuedBytes += file.size;
        setPendingReads((n) => n + 1);
        const reader = new FileReader();
        reader.onload = () => {
          const input = new AttachmentInput();
          input.name = file.name;
          // infer mime when empty via extension
          let mime = file.type;
          if (!mime) {
            const ext = getExt(file.name);
            if (ext === "json") mime = "application/json";
            else if (ext === "csv") mime = "text/csv";
            else if (ext === "pdf") mime = "application/pdf";
            else if (ext === "md" || ext === "mdx") mime = "text/markdown";
            else if (ext === "txt") mime = "text/plain";
            else mime = "application/octet-stream";
          }
          input.mimeType = mime;
          input.data = new Uint8Array(reader.result as ArrayBuffer);
          setAttachments((prev) => (prev.length >= MAX_ATTACHMENTS ? prev : [...prev, input]));
          setPendingReads((n) => Math.max(0, n - 1));
        };
        reader.onerror = () => {
          useToastStore.getState().push({ kind: "error", message: `Failed to read ${file.name}` });
          setPendingReads((n) => Math.max(0, n - 1));
        };
        reader.readAsArrayBuffer(file);
      }
      e.target.value = "";
    },
    [attachments],
  );

  const handlePaste = useCallback(
    (e: React.ClipboardEvent<HTMLTextAreaElement>) => {
      const items = e.clipboardData?.items;
      if (!items) return;
      const imageFiles: File[] = [];
      for (let i = 0; i < items.length; i++) {
        const item = items[i];
        if (item.kind === "file" && item.type.startsWith("image/")) {
          const file = item.getAsFile();
          if (file) imageFiles.push(file);
        }
      }
      if (imageFiles.length === 0) return;
      e.preventDefault();
      const existingTotal = attachments.reduce((s, a) => s + (a.data?.length ?? 0), 0);
      let queuedCount = 0;
      let queuedBytes = 0;
      for (const file of imageFiles) {
        if (attachments.length + queuedCount >= MAX_ATTACHMENTS) {
          useToastStore.getState().push({ kind: "error", message: `Too many attachments (max ${MAX_ATTACHMENTS})` });
          break;
        }
        if (file.size > MAX_FILE_BYTES) {
          useToastStore.getState().push({ kind: "error", message: `Image too large (max 10MB): ${file.name || "pasted image"}` });
          continue;
        }
        if (existingTotal + queuedBytes + file.size > MAX_TOTAL_BYTES) {
          useToastStore.getState().push({ kind: "error", message: `Attachments too large (max 20MB total)` });
          continue;
        }
        queuedCount++;
        queuedBytes += file.size;
        setPendingReads((n) => n + 1);
        const reader = new FileReader();
        reader.onload = () => {
          const input = new AttachmentInput();
          input.name = file.name || `pasted-image-${Date.now()}.png`;
          input.mimeType = file.type || "image/png";
          input.data = new Uint8Array(reader.result as ArrayBuffer);
          setAttachments((prev) => (prev.length >= MAX_ATTACHMENTS ? prev : [...prev, input]));
          setPendingReads((n) => Math.max(0, n - 1));
        };
        reader.onerror = () => {
          useToastStore.getState().push({ kind: "error", message: `Failed to read ${file.name || "pasted image"}` });
          setPendingReads((n) => Math.max(0, n - 1));
        };
        reader.readAsArrayBuffer(file);
      }
    },
    [attachments],
  );

  const [dragOver, setDragOver] = useState(false);
  const handleDragOver = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setDragOver(true);
  }, []);
  const handleDragLeave = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setDragOver(false);
  }, []);
  const handleDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault();
      e.stopPropagation();
      setDragOver(false);
      const files = e.dataTransfer?.files;
      if (!files) return;
      const existingTotal = attachments.reduce((s, a) => s + (a.data?.length ?? 0), 0);
      let queuedCount = 0;
      let queuedBytes = 0;
      for (const file of Array.from(files)) {
        if (attachments.length + queuedCount >= MAX_ATTACHMENTS) {
          useToastStore.getState().push({ kind: "error", message: `Too many attachments (max ${MAX_ATTACHMENTS})` });
          break;
        }
        if (!isAllowedFile(file)) {
          useToastStore.getState().push({ kind: "error", message: `Unsupported file type: ${file.name}` });
          continue;
        }
        if (file.size > MAX_FILE_BYTES) {
          useToastStore.getState().push({ kind: "error", message: `File too large (max 10MB): ${file.name}` });
          continue;
        }
        if (existingTotal + queuedBytes + file.size > MAX_TOTAL_BYTES) {
          useToastStore.getState().push({ kind: "error", message: `Attachments too large (max 20MB total)` });
          continue;
        }
        queuedCount++;
        queuedBytes += file.size;
        setPendingReads((n) => n + 1);
        const reader = new FileReader();
        reader.onload = () => {
          const input = new AttachmentInput();
          input.name = file.name;
          let mime = file.type;
          if (!mime) {
            const ext = getExt(file.name);
            if (ext === "json") mime = "application/json";
            else if (ext === "csv") mime = "text/csv";
            else if (ext === "pdf") mime = "application/pdf";
            else if (ext === "md" || ext === "mdx") mime = "text/markdown";
            else if (ext === "txt") mime = "text/plain";
            else mime = "application/octet-stream";
          }
          input.mimeType = mime;
          input.data = new Uint8Array(reader.result as ArrayBuffer);
          setAttachments((prev) => (prev.length >= MAX_ATTACHMENTS ? prev : [...prev, input]));
          setPendingReads((n) => Math.max(0, n - 1));
        };
        reader.onerror = () => {
          useToastStore.getState().push({ kind: "error", message: `Failed to read ${file.name}` });
          setPendingReads((n) => Math.max(0, n - 1));
        };
        reader.readAsArrayBuffer(file);
      }
    },
    [attachments],
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
        if (blob.size > MAX_FILE_BYTES) {
          pushToast({ kind: "error", message: `Audio too large (max 10MB)` });
          return;
        }
        const existingTotal = attachments.reduce((s, a) => s + (a.data?.length ?? 0), 0);
        if (existingTotal + blob.size > MAX_TOTAL_BYTES) {
          pushToast({ kind: "error", message: `Attachments too large (max 20MB total)` });
          return;
        }
        if (attachments.length >= MAX_ATTACHMENTS) {
          pushToast({ kind: "error", message: `Too many attachments (max ${MAX_ATTACHMENTS})` });
          return;
        }
        setPendingReads((n) => n + 1);
        const reader = new FileReader();
        reader.onload = () => {
          const input = new AttachmentInput();
          input.name = "voice_input.webm";
          input.mimeType = "audio/webm";
          input.data = new Uint8Array(reader.result as ArrayBuffer);
          setAttachments((prev) => (prev.length >= MAX_ATTACHMENTS ? prev : [...prev, input]));
          setPendingReads((n) => Math.max(0, n - 1));
          pushToast({
            kind: "success",
            message:
              "Audio recorded and attached. Send your message to have Orchicon process it.",
          });
        };
        reader.onerror = () => {
          pushToast({ kind: "error", message: "Failed to read audio" });
          setPendingReads((n) => Math.max(0, n - 1));
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
  }, [attachments]);

  const handleStopRecording = useCallback(() => {
    if (mediaRecorderRef.current?.state === "recording") {
      mediaRecorderRef.current.stop();
    }
  }, []);

  return (
    <div className="p-4">
      {pendingReads > 0 && (
        <div className="mb-2 text-xs text-muted-foreground animate-pulse" aria-live="polite">
          Loading {pendingReads} file(s)...
        </div>
      )}
      {attachments.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-2">
          {attachments.map((a, i) => {
            const isImage = a.mimeType?.startsWith("image/");
            let previewUrl: string | undefined;
            try {
              if (isImage && a.data && a.data.length > 0) {
                // Use data URL (no object URL leak) — small preview, auto-revoked on remove
                let binary = "";
                const bytes = a.data as unknown as Uint8Array;
                for (let k = 0; k < bytes.length; k++) binary += String.fromCharCode(bytes[k]);
                // chunk to avoid stack overflow on large images
                previewUrl = `data:${a.mimeType};base64,${btoa(binary)}`;
              }
            } catch {}
            return (
              <span
                key={i}
                className="inline-flex items-center gap-1 rounded-md bg-muted px-2 py-1 text-xs"
              >
                {isImage && previewUrl ? (
                  <img src={previewUrl} alt={a.name} className="h-8 w-8 rounded object-cover" />
                ) : (
                  <Paperclip className="h-3 w-3" />
                )}
                <span className="max-w-[120px] truncate">{a.name}</span>
                <span className="text-muted-foreground">{a.data ? `${(a.data.length / 1024).toFixed(1)}KB` : ""}</span>
                <button
                  onClick={() =>
                    setAttachments((prev) => prev.filter((_, j) => j !== i))
                  }
                  className="ml-1 text-muted-foreground hover:text-foreground"
                >
                  ×
                </button>
              </span>
            );
          })}
        </div>
      )}
      <div
        className={`glass-input rounded-2xl p-2.5 overflow-hidden ${dragOver ? "ring-2 ring-cyan-400 border-dashed" : ""}`}
        onDragOver={handleDragOver}
        onDragLeave={handleDragLeave}
        onDrop={handleDrop}
      >
        {/* Textarea area */}
        <div className="flex items-end gap-2 px-1 pb-1">
          <Sparkles className="h-4 w-4 text-cyan-400 shrink-0 mb-1.5" />
          <button
            type="button"
            onClick={() => fileInputRef.current?.click()}
            className="shrink-0 rounded-md border border-black/10 dark:border-white/10 bg-black/[0.04] dark:bg-white/5 p-1.5 text-muted-foreground hover:text-cyan-600 dark:hover:text-cyan-300 hover:bg-black/5 dark:hover:bg-white/10 transition-colors"
            title="Attach file"
            aria-label="Attach file"
          >
            <Paperclip className="h-4 w-4" />
          </button>
          <textarea
            ref={inputRef}
            value={text}
            onChange={handleTextChange}
            onKeyDown={handleKeyDown}
            onPaste={handlePaste}
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
                  disabled={pendingReads > 0 || (!text.trim() && attachments.length === 0)}
                  size="sm"
                  className="bg-gradient-to-r from-cyan-500 to-blue-600 hover:from-cyan-600 hover:to-blue-700 text-white border-0 disabled:opacity-50"
                  title={pendingReads > 0 ? `Loading ${pendingReads} file(s)...` : "Send interrupts the current reply and redirects the model"}
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
                disabled={pendingReads > 0 || (!text.trim() && attachments.length === 0)}
                size="sm"
                className="bg-gradient-to-r from-cyan-500 to-blue-600 hover:from-cyan-600 hover:to-blue-700 text-white border-0 disabled:opacity-50"
                title={pendingReads > 0 ? `Loading ${pendingReads} file(s)...` : undefined}
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
        accept="image/*,text/*,.json,.md,.mdx,.csv,.pdf,.txt,.yaml,.yml,.html,.css,.js,.ts,.tsx,.go,.py,.rs,.java,.sh,.xml,.log"
        multiple
        onChange={handleFileSelect}
        aria-hidden="true"
        tabIndex={-1}
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
          "w-full text-left rounded-xl p-2.5 text-sm transition border",
          isActive
            ? "bg-cyan-500/10 border-cyan-500/30 text-white"
            : "border-transparent text-slate-300 hover:bg-white/5 hover:border-white/5 hover:text-white",
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
