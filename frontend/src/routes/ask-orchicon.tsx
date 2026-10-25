import { createRoute, useNavigate } from "@tanstack/react-router";
import { useState, useCallback, useRef, useEffect, useMemo, type MutableRefObject } from "react";
import {
  Plus,
  Trash2,
  Paperclip,
  Mic,
  Minimize2,
  Loader2,
  Square,
  RefreshCw,
  Pencil,
  FolderPlus,
  ChevronRight,
  GripVertical,
  MessageSquare,
  PanelRight,
  PanelRightClose,
  PanelLeft,
  PanelLeftClose,
  History,
  Sparkles,
} from "lucide-react";

import { Link } from "@tanstack/react-router";
import { Route as rootRoute } from "@/routes/__root";

import { useQueryClient } from "@tanstack/react-query";
import { AskModelChip } from "@/components/AskModelChip";
import { Button } from "@/components/ui/button";
import { LiveDuration } from "@/components/ui/live-duration";
import { ModeIcon } from "@/components/ui/ModeIcon";
import { ModeToggle } from "@/components/ui/mode-toggle";
import { FullsendToggle } from "@/components/ui/fullsend-toggle";
import { useAskMetricsLive } from "@/lib/ask-metrics";
import { cn } from "@/lib/utils";
import { ProjectScopeSelect } from "@/components/conversations/ProjectScopeSelect";
import {
  ALL_PROJECTS,
  categoriesForScope,
  composerProjectChoices,
  effectiveComposerProject,
  filterConversationsByScope,
  scopeLabel,
  scopeOptions,
  type ScopeOption,
} from "@/lib/conversationProjects";
import { conversationModeLabel, conversationModeMeta } from "@/lib/conversationModes";
import { runningConvIds } from "@/lib/ask-running";
import { filterByQuery } from "@/lib/transcript-search";
import { extrapolateServerTime } from "@/lib/ask-verbs";
import {
  activityLineAnnouncement,
  activityLineFor,
  type ActivityLineInput,
} from "@/lib/ask-activity-notice";
import { transcriptPollMs } from "@/lib/ask-transcript-poll";
import { toolCallsFromMessages, type CountableMessage } from "@/lib/ask-tool-summary";
import { useRailWidth } from "@/lib/diff/useRailWidth";
import {
  useListConversations,
  useCreateConversation,
  useDeleteConversation,
  useUpdateConversationTitle,
  useListMessages,
  usePendingAsks,
  useGetConversation,
  useAbortConversationTurn,
  useSetConversationMode,
  useSetConversationModel,
  useSetConversationFullsend,
  useSetConversationProject,
  useCompactConversation,
  askKeys,
} from "@/api/askOrchicon";
import { useGetSettings } from "@/api/settings";
import { askOrchiconClient } from "@/api/clients";
import { useToast, useToastStore } from "@/components/ui/toast";
import { COMPACT_COMMAND, parseComposerCommand } from "@/lib/composer-command";
import { bubbleKindFor } from "@/lib/ask-bubble";
import {
  dropSupersededMirror,
  hasLiveTextItems,
  mirrorRowProseLifted,
  orderInjectedBeforeReply,
} from "@/lib/ask-live-merge";
import {
  groupStreamItems,
  nextChunkKey,
  type StreamItem,
} from "@/lib/ask-stream-group";
import { ConversationMode } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import {
  PermissionChoice,
  type PermissionAsk,
} from "@/api/gen/orchicon/api/v1/ask_orchicon_service_pb";
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
  NoticeBubble,
  ChatScrollContainer,
  ActivityLine,
  TranscriptSearchBar,
} from "@/components/chat";
import { useCategoryPreferences, getItemsForCategory } from "@/lib/category-store";
import { AskCard, isAskUserToolCall, parseAskUserArgs } from "@/components/ask/AskCard";
import { ConsentAskCard } from "@/components/ask/AskCard";
import {
  applyAskChunk,
  askCardPlan,
  interleave,
  settleFromLedger,
  outcomeFromChoice,
  outcomeFromWire,
  pendingFor,
  resolveAsk,
  type AskItem,
} from "@/lib/ask-consent";
import { SessionGrants } from "@/components/ask/SessionGrants";
import { ConversationScopeDisclosure } from "@/components/ask/ConversationScopeDisclosure";
import { CreateCategoryDialog } from "@/components/CreateCategoryDialog";
import { DiffSidebar, type DiffTab } from "@/components/diffs/DiffSidebar";
import { usePersistentState } from "@/lib/diff/usePersistentState";
import { RAIL_DEFAULT_WIDTH } from "@/lib/diff/railResize";
import {
  DndContext,
  DragOverlay,
  PointerSensor,
  pointerWithin,
  useSensor,
  useSensors,
  type DragEndEvent,
  type DragOverEvent,
} from "@dnd-kit/core";
import { useDroppable } from "@dnd-kit/core";
import { useListProjects } from "@/api/projects";
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

// --- streaming item type (extracted: @/lib/ask-stream-group) ---
// groupStreamItems coalesces only CONSECUTIVE same-kind chunks so
// interleaved text/reasoning arrivals render in arrival order.

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
  // turnProgressing is the server-confirmed "still genuinely working" signal
  // (Conversation.turn_progressing) captured when a refresh re-attaches the
  // reconnecting slot. True => show "still working…"; false (while reconnecting)
  // => the turn is stalled/wedged, so show an accurate "stalled" state instead
  // of the misleading "connection lost — still working".
  turnProgressing: boolean;
  // serverTimeMs is the SERVER's clock as last heard (Heartbeat.server_time_unix_ms), and
  // serverTimeRecvAt is the LOCAL instant it arrived. Together they index the rotating activity
  // verb (see @/lib/ask-verbs): the word is a pure function of server time, so two clients draw the
  // same word with no shared state and no new RPC — and because only the DELTA since our own receipt
  // is added, a skewed local clock never changes the word. null means "no heartbeat yet": the render
  // falls back to the list's first word rather than to an empty line.
  //
  // BOTH heartbeat arms must feed this — the dispatch stream and the re-dialled watch stream. A
  // watcher that re-attached mid-turn is the client LEAST likely to be holding a stamp.
  serverTimeMs: number | null;
  serverTimeRecvAt: number | null;
  optimisticUserMsg: string | null;
  pendingReplyId: string | null;
  // sentText is the message text captured at send time, held in-memory so the
  // completion effect can copy it to the clipboard if the turn is ACKED but
  // the reply later fails. Cleared on completion (success or failure) — it is
  // not a persisted draft, so it never resurrects into the composer.
  sentText: string | null;
  items: StreamItem[];
  // asks are the consent cards this conversation has seen, PENDING OR SETTLED.
  // They live OUTSIDE `items` on purpose: `items` is emptied by every
  // turn-lifecycle updater, and the outcome of an ask must stay in the
  // transcript (an operator scrolling back must see that a grant was given and
  // what it covered). Nothing in this file clears `asks` except switching
  // conversation, which is per-slot anyway.
  asks: AskItem[];
}

/**
 * One block of the transcript flow a consent card is interleaved into.
 *
 * A message carries the server's createdAt; the optimistic echo carries the
 * moment it was sent. Both are epoch ms, which is the unit the interleave sorts
 * by — and the unit an AskItem now carries too, so a card, a message and an echo
 * are ordered by the same clock rather than by where they happened to be
 * rendered.
 */
type TranscriptBlock =
  | { kind: "message"; at: number; message: ChatMessage }
  | { kind: "optimistic"; at: number; text: string };

/**
 * One slot in the interleaved transcript: a message/echo block or a consent card.
 * It mirrors the element type interleave returns.
 */
type TranscriptSlot = {
  seq: number;
  at: number;
  block: TranscriptBlock | AskItem;
  isAsk: boolean;
};

/**
 * slotSearchText is the text an interleaved slot is searched over. A consent card
 * is not a message, so it carries no searchable text — such slots are RETAINED by
 * the filter's keep predicate (below) rather than matched, so a search can never
 * hide a pending approval.
 */
function slotSearchText(slot: TranscriptSlot): string {
  if (slot.isAsk) return "";
  const block = slot.block as TranscriptBlock;
  return block.kind === "optimistic" ? block.text : block.message.content;
}
const EMPTY_STREAM: ConvStream = {
  isStreaming: false,
  isThinking: false,
  reconnecting: false,
  turnProgressing: false,
  serverTimeMs: null,
  serverTimeRecvAt: null,
  optimisticUserMsg: null,
  pendingReplyId: null,
  sentText: null,
  items: [],
  asks: [],
};

// THERE IS NO LAST-RESORT MODEL, because every candidate was a lie.
//
// This mirrored the server's old hardcoded fallback, `opencode/deepseek-v4-flash-free`, and the
// operator reported what it actually did:
//
//	conversation session send: orchicon bridge: start Ask turn: provider status 403 403 Forbidden:
//	{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used
//	from within OpenCode. Download it at https://opencode.ai and keep building."}}
//
// The ref was a legacy 2-segment one (so it INFERS the opencode adapter, which a fresh install has no
// binary for) naming a FREE-TIER-ONLY model (which opencode gates on the request coming from inside
// its own client — the native adapter is refused too, since it calls the same endpoints with the same
// key). Both halves are unfixable without the opencode binary, so inventing a model here could only
// ever produce an error the operator cannot act on.
//
// The server now REFUSES such a turn with a FailedPrecondition naming the setting to fix (see
// askorchicon/chat.go). The UI shows "no model configured" — a true, fixable statement — instead of a
// model name that cannot dispatch.
const NO_MODEL_CONFIGURED_LABEL = "no model configured";

// Ticks Date.now() every `intervalMs` while truthy, frozen otherwise. Used to
// render live "last activity Ns ago" elapsed time in the reconnecting banner
// without running a timer (and re-rendering the tree) when nothing needs it.
function useNow(intervalMs: number | false): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!intervalMs) return;
    const t = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(t);
  }, [intervalMs]);
  return now;
}

// "12s" / "3m 05s" / "1h 12m" — elapsed-time label for the activity ticker.
function formatElapsed(totalSecs: number): string {
  const s = Math.max(0, Math.floor(totalSecs));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${String(m % 60).padStart(2, "0")}m`;
}

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

  // THE PROJECT A NOT-YET-CREATED CONVERSATION WILL BE CREATED IN, chosen from the composer.
  //
  // THE GAP THIS CLOSES. The GUI had NO way to set a conversation's project at creation: the only
  // control was the rail's per-row "move to project", which is reachable only AFTER the chat exists.
  // So a conversation you meant to file in a workspace was born unassigned, and filing it happened as
  // a second, separate action — which is exactly the sequence the operator described ("you have to
  // chat first, then go over and change it in the drop down once it's in the rail").
  //
  // The TUI does not have this problem because it INFERS the workspace from the directory it was
  // launched in. That inference is not something this client can port faithfully — it has no launch
  // directory — but choosing it explicitly at the point of use is the same power, and it is how the
  // MODE already works on this row (see localMode above): held here before a conversation exists,
  // handed to the create call, retargeted directly once there is a row to write to.
  //
  // Empty means "not chosen yet", which falls back to the rail's scope (scopeProjectId) so the
  // composer can never silently disagree with the workspace the sidebar is showing.
  const [composerProjectId, setComposerProjectId] = useState<string | null>(null);

  // The model a NOT-YET-CREATED conversation will be created with. The chat box
  // shows an "Ask Orchicon Anything..." hero before any conversation exists, and
  // the operator must be able to choose the model THERE too — so with no
  // conversation open the choice is held here and handed to createConversation,
  // rather than being lost (there is no conversation row to write it to yet).
  // Once a conversation exists it is retargeted directly and this is unused.
  const [pendingModel, setPendingModel] = useState("");

  // Live streaming state keyed by conversation id.
  const [streams, setStreams] = useState<Record<string, ConvStream>>({});

  // Per-conversation dispatch generation. Each send/interject bumps it; a
  // stream's fail() only owns the slot while its generation is current. This
  // stops a superseded (older) turn's socket-drop from tearing down the
  // interject turn that replaced it — the classic stale-closure hazard once
  // two streams can overlap for a conversation.
  const dispatchGenRef = useRef<Record<string, number>>({});
  // restoredWatchRef marks a conversation whose slot was RESTORED by the
  // re-attach effect (rather than dispatched locally), so the watch re-dial
  // runs for exactly that case and never double-dials a turn we dispatched.
  const restoredWatchRef = useRef<Record<string, boolean>>({});

  // Per-conversation monotonic chunk sequence. Live TextChunk/Reasoning
  // chunks are keyed `st-<seq>` / `sr-<seq>` (never Math.random()) so
  // React reconciliation never reshuffles bubbles on re-render.
  const chunkSeqRef = useRef<Record<string, number>>({});

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
      } catch { /* storage may be disabled (private mode) — collapse state is best-effort */ }
      return next;
    });
  }, []);

  // Diff sidebar state — persisted per page so it survives navigation within
  // the session (per acceptance criteria). Closed is the default for
  // first-time users (usePersistentState reads the localStorage default).
  const [diffOpen, setDiffOpen] = usePersistentState("ask-orchicon:open", false);
  const [diffTab, setDiffTab] = usePersistentState<DiffTab>("ask-orchicon:tab", "diff");
  const [diffPath, setDiffPath] = usePersistentState("ask-orchicon:selectedPath", "");
  // Rail width — same per-page persistence discipline as open/tab/selectedPath,
  // under its OWN key so this page and the execution page cannot overwrite each
  // other's width. `diffRowRef` is the flex row the rail is the first child of;
  // the rail clamps its width against that row's measured width so the chat
  // column beside it can never be squeezed out.
  const [railWidth, setRailWidth] = usePersistentState<number>(
    "ask-orchicon:railWidth",
    RAIL_DEFAULT_WIDTH,
  );
  const diffRowRef = useRef<HTMLDivElement>(null);
  const toggleDiffSidebar = useCallback(() => {
    setDiffOpen((prev) => !prev);
  }, [setDiffOpen]);
  const [mobileSheetOpen, setMobileSheetOpen] = useState(false);
  const mobileTriggerRef = useRef<HTMLButtonElement | null>(null);
  const lastMobileTriggerRef = useRef<HTMLButtonElement | null>(null);
  const openMobileSheet = useCallback(() => {
    lastMobileTriggerRef.current = mobileTriggerRef.current;
    setMobileSheetOpen(true);
  }, []);
  const closeMobileSheet = useCallback(() => {
    setMobileSheetOpen(false);
    requestAnimationFrame(() => {
      (lastMobileTriggerRef.current ?? mobileTriggerRef.current)?.focus();
    });
  }, []);
  useEffect(() => {
    if (!mobileSheetOpen) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setMobileSheetOpen(false);
        requestAnimationFrame(() => {
          (lastMobileTriggerRef.current ?? mobileTriggerRef.current)?.focus();
        });
      }
    };
    document.addEventListener("keydown", onKeyDown);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKeyDown);
      document.body.style.overflow = prev;
    };
  }, [mobileSheetOpen]);

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

  // applyAsk folds a streamed PermissionAsk into its conversation's transcript.
  // Deduped by ask id (the live socket and the re-dialled watch socket can both
  // deliver it), and never resurrected once settled.
  const applyAsk = useCallback(
    (convId: string, ask: PermissionAsk) => {
      setStream(convId, (prev) => ({ ...prev, asks: applyAskChunk(prev.asks, ask) }));
    },
    [setStream],
  );

  // The ACTIVE conversation's derived streaming state.
  const activeStream = activeConvId ? streams[activeConvId] : undefined;
  const isStreaming = activeStream?.isStreaming ?? false;
  const isThinking = activeStream?.isThinking ?? false;
  const reconnecting = activeStream?.reconnecting ?? false;
  const turnProgressing = activeStream?.turnProgressing ?? false;
  const optimisticUserMsg = activeStream?.optimisticUserMsg ?? null;
  const pendingReplyId = activeStream?.pendingReplyId ?? null;
  const streamItems = activeStream?.items ?? [];

  // Group streaming items into coalesced bubbles.
  //
  // DECLARED HERE, ABOVE ITS FIRST CONSUMER, and the move is load-bearing: the transcript builder
  // below needs it to decide whether the live stream is already drawing the running reply (see
  // lib/ask-live-merge), and this file's own rule is that a `const` used above its declaration is a
  // TDZ error the production build rejects (the same reason `messages` and the turn union were
  // moved up). It depends on nothing but `streamItems`, which is the line above.
  const groupedStream = useMemo(
    () => groupStreamItems(streamItems),
    [streamItems],
  );

  // THE TURN'S IN-FLIGHT UNION IS DECLARED HERE, ABOVE ITS FIRST CONSUMER, and the move is a
  // pure reordering: `listPollMs` is already in scope (state, declared with the other list-poll
  // state), and nothing between here and their old site read `conversations`/`activeConv`. TWO
  // consumers now need it — the transcript's refetch cadence immediately below, and the activity
  // line's ticker further down — and this file's own rule is that a `const` used above its
  // declaration is a TDZ error the production build rejects (see the note on `messages`).
  const { data: conversations, isLoading: convsLoading } =
    useListConversations({
      // Poll while ANY conversation has a running turn so the sidebar's
      // running indicators + Stop buttons stay live across tabs and devices
      // (a turn started elsewhere appears within a few seconds), and stop
      // polling once everything settles — no idle network churn.
      refetchInterval: listPollMs,
    });
  const { data: activeConv } = useGetConversation(activeConvId ?? "");

  // Server-reported turn state for the ACTIVE conversation, freshest source
  // first. The conversations list polls every 3s while any turn is running;
  // GetConversation is fetched once per conversation, so it only fills gaps
  // before the first poll lands.
  const serverTurnInFlight =
    conversations?.find((c) => c.id === activeConvId)?.turnInFlight ??
    activeConv?.turnInFlight ??
    false;
  // A TURN IS IN FLIGHT FOR THIS CONVERSATION — EITHER HALF, and both are load-bearing:
  // `isStreaming` is this client's own live slot (which supplies the local age), and
  // `serverTurnInFlight` covers a turn this client is not streaming (started in another tab, or a
  // slot lost on reload). The same union gates the TUI's status line (internal/tui/app.go
  // transcriptStatusLine: `IsStreaming(conv) || turnInFlight(conv)`), and keying it to the slot
  // alone was the contradiction the operator reported: "After the initial 'Orchicon is
  // thinking...', streaming started and the 'Orchicon is thinking...' went away and never came
  // back." This is the whole regression fix — the activity line is gated on the TURN, not on
  // "before the first token".
  const turnInFlight = isStreaming || serverTurnInFlight;

  // MESSAGES IS DECLARED BEFORE ITS CONSUMERS. `transcriptBlocks` below reads it,
  // and a `const` used above its declaration is a TDZ error the production build
  // (`tsc -b`) rejects even though a loose `--noEmit` pass did not — which is how
  // it reached a release build.
  const { data: messages, isLoading: msgsLoading } = useListMessages(
    activeConvId ?? "",
    // THE CADENCE FOLLOWS THE TURN, NOT THIS CLIENT'S STREAM SLOT. Gated on `isStreaming`
    // alone, a turn this client is not streaming — started in the TUI or another tab, or lost to
    // a reload — left this page frozen at whatever it held on load while the line's ticker kept
    // running: "the timer that shows when the last call occurred just continues counting up and
    // never resets on the next newest call". The ledger cannot refresh itself; the poll is the
    // whole mechanism. See lib/ask-transcript-poll.ts.
    { refetchInterval: transcriptPollMs(turnInFlight) },
  );

  // THE SERVER'S TRUTH SETTLES THE CARD, not only the live stream event.
  //
  // A `PermissionAskResolved` event reaches only a client watching the turn at that moment, so
  // answering in the TUI left a card up in the GUI, in a second tab, and after a reload — which
  // is why fixing it in the stream kept "working" and kept coming back. The operator's rule is
  // the right one: "the truth should be set server side and update all clients when a response
  // is seen whether it's TUI, GUI, separate tabs/browsers."
  //
  // The server writes every consent decision into the turn's ledger as a `permission.<verdict>`
  // record whose ID is the ASK ID, and the ledger is persisted with the assistant message. So
  // the transcript IS the truth, it survives a restart, and every client already fetches it. Any
  // card whose ask appears there resolved is settled here — on load, on every poll, in every tab.
  useEffect(() => {
    if (!activeConvId || !messages || messages.length === 0) return;
    const current = streams[activeConvId]?.asks;
    if (!current || current.length === 0) return;
    // untouched when there is nothing to settle, so this never churns state or re-renders.
    if (settleFromLedger(current, messages) === current) return;
    setStream(activeConvId, (prev) => ({ ...prev, asks: settleFromLedger(prev.asks, messages) }));
  }, [activeConvId, messages, streams, setStream]);

  // DISCOVERY, beside the ledger settle above. The ledger can retire a card but can
  // never reveal an open one (it records outcomes, not open asks), so this asks the
  // server directly — the same call the TUI makes on attach and re-attach.
  //
  // It is the half that makes the GUI robust rather than merely lucky: today it
  // usually holds a watch socket, so it usually receives the live arm. "Usually" is
  // what the operator caught, from the other side — the TUI appeared stalled while
  // this client was quietly displaying the waiting card. Both clients now reconcile
  // against the server's pending set, so neither depends on having caught an event.
  // GATED ON THE TURN UNION, not on this client's stream slot. A card can be raised by a turn this client
  // never streamed (started in the TUI or another tab, or one whose socket dropped), which is precisely the
  // case the discovery read exists for — gating it on our own slot would leave it asleep in that case, i.e.
  // broken in exactly the situation it is needed. `turnInFlight` is declared above.
  const { data: pendingAsks } = usePendingAsks(activeConvId ?? "", {
    refetchInterval: transcriptPollMs(turnInFlight),
  });
  useEffect(() => {
    if (!activeConvId || !pendingAsks || pendingAsks.length === 0) return;
    // Through the SAME fold the wire arm uses, so a card delivered both ways is drawn
    // once (applyAskChunk dedupes by ask id) and renders identically either way.
    for (const ask of pendingAsks) {
      // The message is the SAME proto type the stream arm delivers, so it is folded in
      // unchanged — no reshaping, which is what keeps a discovered card identical to a
      // streamed one. Only a producer that omitted the conversation id is corrected.
      if (!ask.conversationId) ask.conversationId = activeConvId;
      applyAsk(activeConvId, ask);
    }
  }, [activeConvId, pendingAsks, applyAsk]);

  // transcriptBlocks is the message flow the cards are interleaved into. The
  // optimistic echo is only included while the durable view has not caught up
  // with it (the same rule the old inline render used), so it cannot double.
  const showOptimistic =
    !!optimisticUserMsg &&
    !messages?.some((m) => m.content === optimisticUserMsg && m.role === "user");
  // THE RUNNING REPLY ARRIVES TWICE, so exactly ONE copy is drawn. The server streams it as it is
  // written AND mirrors it into the acked row (so a client with no live stream still sees it).
  // While this client holds live text/reasoning, the live copy is the one that grows and the
  // in-flight row is skipped; with no live items the row is the fallback, and after the turn the
  // live items are cleared and the row (now the complete reply) is the only copy. See
  // lib/ask-live-merge.ts — and note this must hold for BOTH the transcript and the scroll dep
  // below, or the reply is drawn once and counted twice.
  // IN-TRANSCRIPT SEARCH, declared HERE because the supersession decision below reads it. (The filter
  // helpers it feeds are unit-tested in @/lib/transcript-search; the interleaved transcript is built once
  // so the filter and the render cannot disagree about slot order.)
  const [searchQuery, setSearchQuery] = useState("");
  const [activeMatch, setActiveMatch] = useState(0);
  const searchActive = searchQuery.trim() !== "";

  // AND IT MUST ACCOUNT FOR SEARCH. The live items are NOT DRAWN while a search filter is active — the
  // render gates them on `!searchActive` so the view shows only matches — so treating them as "present"
  // would suppress the mirror row while nothing replaced it, and an in-flight reply would DISAPPEAR the
  // moment the operator opened the find box. Measured in a browser: the transcript went to "No matches"
  // with the streamed text still arriving. The supersession rule is about what is ON SCREEN, so it asks
  // whether the live copy is being drawn, not merely whether it exists.
  const hasLive = hasLiveTextItems(groupedStream);
  // AND THE RULE IS THE SAME UNDER SEARCH. An earlier attempt made this false while searching (to keep the
  // mirror row as a fallback) — but the search then had nothing to filter, because the live blocks below
  // were keyed off this value too. Making it unconditional keeps ONE decision: while live text exists it is
  // the copy both DRAWN and SEARCHED; the mirror row is the fallback for a client with no live items.
  const liveItemsPresent = hasLive;
  // groupedStream DOES need to be declared before this use; it is a `const` above (useMemo), and
  // this file's own rule is that a const used above its declaration is a TDZ error the production
  // build rejects.
  // INTERJECTIONS ARE REORDERED BEFORE THE MIRROR IS CHOSEN.
  //
  // An interjected user message is persisted MID-TURN, so its created_at is later than the assistant row
  // that was created when the turn started — which made the operator's message render BELOW the reply it
  // prompted. orderInjectedBeforeReply puts it in front of that reply, where it reads as a chat message the
  // model then answered. Done here, on the durable list, because every transcript consumer (the render, the
  // search blocks and the mirror selection below) reads this one value; reordering later would leave one of
  // them disagreeing.
  const durableMessages = orderInjectedBeforeReply(
    dropSupersededMirror(messages ?? [], pendingReplyId, isStreaming, liveItemsPresent),
  );

  // A LIVE-ONLY TURN MUST STILL BE SEARCHABLE.
  //
  // While the mirror row is suppressed in favour of the live stream items, the reply exists in
  // `groupedStream` and NOT in `messages` — but the transcript renders it from `visibleSlots`, which is
  // built from `transcriptBlocks` (the durable messages). So a search during an in-flight turn would find
  // NOTHING: the very text on screen is not among the blocks being filtered. Measured in a browser: an
  // active search reported "No matches" while the reply was visibly streaming.
  //
  // Normalized here rather than suppressed in the search, so the filter sees exactly ONE copy of the text
  // — the same copy the supersession rule chose to draw. A turn whose transcript is complete (no live
  // items) produces an empty list and changes nothing, which is the common case.
  // The live copy as SEARCHABLE blocks, so the text an operator is looking at during an in-flight turn can
  // be found. It exists because the transcript renders `visibleSlots` (built from the durable messages)
  // while the running reply is drawn from `groupedStream` — so on a live turn the visible text is not among
  // the blocks the filter sees, and a search found nothing at all. Measured in a browser: "No matches" while
  // the reply streamed on screen.
  //
  // IT IS EXACTLY ONE COPY. The mirror row is dropped whenever this list is non-empty (the same
  // supersession rule the render uses), so the search never sees the turn twice.
  // THE CARD THAT MUST BE DRAWN AFTER THE LIVE TEXT.
  //
  // The in-flight row carries the recorded ask_user call, and while a reply streams that row sorts BEFORE the
  // live text (it predates it) — so the card, drawn as the last thing inside the row, lands between the row's
  // prose and the live copy. The operator: "the card IS there. However, it's at the top of the text that was
  // sent rather than at the bottom after the text."
  //
  // So it is LIFTED OUT of the row and emitted after the live items, which is where it belongs: the newest
  // thing in the turn, and the thing being asked of the operator. Only while live items exist — with no live
  // stream the row IS the turn (a reload, another tab), and the card draws in its own place.
  const inFlightMsg = durableMessages.find((m) => m.id === pendingReplyId);
  const liftedCard = hasLive && inFlightMsg ? askCardOf(inFlightMsg) : null;

  const liveBlocks: TranscriptBlock[] = hasLive
    ? groupedStream
        .filter((it) => it.kind === "text" || it.kind === "user")
        .map((it) => ({
          kind: "message" as const,
          at: it.at,
          message: { id: `live-${it.key}`, role: "assistant", content: it.text } as ChatMessage,
        }))
    : [];
  const transcriptBlocks: TranscriptBlock[] = [];
  for (const msg of durableMessages) {
    transcriptBlocks.push({
      kind: "message",
      at: Number(msg.createdAt?.seconds ?? 0) * 1000,
      message: msg,
    });
  }
  if (showOptimistic) {
    // Stamped LAST so the echo lands at the end of what has arrived, which is
    // where a just-sent message belongs.
    transcriptBlocks.push({ kind: "optimistic", at: Date.now(), text: optimisticUserMsg! });
  }

  // IN-TRANSCRIPT SEARCH (acceptance: filter matching messages case-insensitively,
  // show a match count, step with prev/next, an explicit no-match state, and clearing
  // restores the full transcript). The query is local UI state and the filtering is
  // the unit-tested helpers in @/lib/transcript-search — client-side over the
  // already-loaded messages only (no server search, no regex, no persisted history:
  // all out of scope). The interleaved transcript is built ONCE here so the filter
  // and the render cannot disagree about slot order.
  // The live blocks ride along ONLY while a search is active. Without a search the live copy is already
  // painted from `groupedStream` (the render's own live-item map), so adding it here too would draw every
  // streaming reply twice; WITH a search that render is gated off and these blocks are the only way the
  // text the operator is looking at can be searched.
  // AND THE SEARCH MUST NOT SEE A LIFTED PROSE TWICE EITHER. A row whose prose is lifted is still in the
  // transcript (for its card and tool calls), but its text is drawn — and therefore searched — from the
  // live copy. Left in, a live turn would report every match twice and next/prev would step onto the
  // same words in two invisible copies.
  const searchBlocks = searchActive
    ? [
        ...transcriptBlocks.filter(
          (b) =>
            !(
              b.kind === "message" &&
              mirrorRowProseLifted(b.message, pendingReplyId, isStreaming, liveItemsPresent)
            ),
        ),
        ...liveBlocks,
      ]
    : transcriptBlocks;

  // AN UNANSWERED QUESTION IS NOT A TRANSCRIPT ENTRY — IT BELONGS AT THE BOTTOM.
  //
  // Cards used to be interleaved by arrival time like any other block, which is right for a SETTLED record
  // ("an operator scrolling back must see that a grant was given and what it covered") and WRONG for an open
  // question. The interleave puts every slot in time order and the live text is rendered AFTER all slots, so a
  // question raised mid-reply was drawn above the very text it was about:
  //
  //   the operator: "The Ask card is on TOP of the text. A question shouldn't come before the information. It
  //   goes information -> Question. It should be at the bottom of the screen."
  //
  // So the two are split by whether the operator still has to act:
  //
  //   PENDING (outcome === null) — drawn LAST, after the live text and after everything else. It is the live
  //     end of the conversation: the turn is parked on it, and it is the one thing being asked of them.
  //   SETTLED — stays interleaved where it happened, because that is history now and history reads in order.
  //
  // When a question is answered it moves to the settled side and takes its place in the timeline, so a
  // scroll-back still shows what was asked and what was chosen.
  const asksAll = activeStream?.asks;
  const settledAsks = (asksAll ?? []).filter((a) => a.outcome !== null);
  const openAsks = (asksAll ?? []).filter((a) => a.outcome === null);

  const transcriptSlots: TranscriptSlot[] = interleave(searchBlocks, settledAsks);
  const visibleSlots = filterByQuery(
    transcriptSlots,
    searchQuery,
    slotSearchText,
    (slot) => slot.isAsk,
  );
  const searchMatchCount = searchActive
    ? visibleSlots.filter((slot) => !slot.isAsk).length
    : 0;
  // Refs to the rendered match wrappers, so next/prev can bring the current match
  // into the middle of the viewport.
  const searchMatchRefs = useRef<(HTMLDivElement | null)[]>([]);
  useEffect(() => {
    if (!searchActive || searchMatchCount === 0) return;
    searchMatchRefs.current[activeMatch]?.scrollIntoView({
      behavior: "smooth",
      block: "center",
    });
  }, [activeMatch, searchActive, searchMatchCount, searchQuery]);

  // The search field and the composer, so Cmd/Ctrl+F can focus the first and the
  // search's Escape can hand focus back to the second.
  const searchInputRef = useRef<HTMLInputElement>(null);
  const composerInputRef = useRef<HTMLTextAreaElement | null>(null);

  // CMD/CTRL+F IS FIND, NOT THE BROWSER'S. The universal search chord opens THIS
  // conversation's transcript search instead of the browser's native find (the
  // Slack / VS Code convention): preventDefault suppresses the native dialog and we
  // focus the field. This is the route's FIRST modifier chord, so it is handled
  // inline rather than in a chord table. The document Escape listener below returns
  // early on metaKey/ctrlKey, so the two never fight over the chord.
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== "f" || e.altKey || !(e.metaKey || e.ctrlKey)) return;
      e.preventDefault();
      searchInputRef.current?.focus();
      searchInputRef.current?.select();
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, []);

  const { data: settings } = useGetSettings();

  // THE RAIL'S RUNNING SET IS A UNION OF BOTH HALVES, not the polled field alone. The row used to read
  // `conv.turnInFlight` by itself, so a conversation THIS client is streaming still read as idle until the
  // next list poll caught up — the operator's "the 'running' status on the conversation rail list doesn't
  // always show up on active running conversations." See lib/ask-running.ts for why it is a union (a turn
  // started in the OTHER client has no local slot and must still mark) and not a local-only check.
  const runningIds = useMemo(
    () => runningConvIds(conversations, streams),
    [conversations, streams],
  );

  const turnLastActivityAt =
    conversations?.find((c) => c.id === activeConvId)?.turnLastActivityAt ??
    activeConv?.turnLastActivityAt ??
    null;
  // 1s ticker while the activity verb can advance, the same trick the reconnecting banner uses for
  // its "last activity" line. THE VERB IS ANIMATION, so it needs a repaint between the 15s heartbeats:
  // the word itself is computed from SERVER time (see activityVerb), but `liveNow` supplies only the
  // delta since the last heartbeat, which is what makes it move without a new packet.
  //
  // IT NOW RUNS FOR THE WHOLE TURN, not only while reconnecting or before the first token: `liveNow`
  // is the delta that advances BOTH the rotating verb between the 15s heartbeats AND the line's own
  // silence age. Freezing it the moment content arrived is exactly the bug this task fixes — the line
  // would be correct but motionless.
  const liveNow = useNow(turnInFlight ? 1000 : false);

  // effectiveServerTimeMs is the SERVER's clock, extrapolated to this repaint: the last heartbeat's
  // server_time_unix_ms plus ONLY the delta since we received it (liveNow - serverTimeRecvAt). The delta
  // is what keeps the rotating verb advancing between the 15s heartbeats without a new packet, while a
  // skewed local clock CANCELS OUT of it — only a difference of our own two readings is ever added — so
  // two clients holding the same stamp draw the same word. 0 is the pre-heartbeat sentinel (no stamp
  // yet), which activityVerb turns into the list's first word rather than an empty line. The rule lives
  // in @/lib/ask-verbs so it is unit-testable (both the skew-cancellation and the pre-heartbeat
  // fallback are asserted there) rather than reachable only through this very large route.
  const effectiveServerTimeMs = extrapolateServerTime(
    activeStream?.serverTimeMs ?? null,
    activeStream?.serverTimeRecvAt ?? null,
    liveNow,
  );

  // THE COUNTER IS A PURE RENDER OVER DATA ALREADY IN HAND. The durable ledger of this turn's tool
  // calls arrived on the ListMessages page the transcript was built from — the same page
  // internal/tui/chat/pageToolCalls reads on the Go side — on the poll that is already running (2s
  // while streaming). So the summary costs NO fetch of its own, and ConvStream stays free of summary
  // state. `messages` is used rather than the streamed items on purpose: the stream cannot see a tool
  // call this client did not witness, and the ledger is the server's record.
  const activityToolCalls = useMemo(
    () => toolCallsFromMessages(messages as unknown as CountableMessage[] | undefined),
    [messages],
  );

  // The line's own ref, so the pane width is measured rather than assumed (fitActivityNotice drops
  // the counters before the verb when the pane is narrow). useRailWidth is the repo's one
  // element-width observer; it returns 0 until measured, which activityLineFor treats as unbounded.
  const activityLineRef = useRef<HTMLDivElement>(null);
  const activityLineWidth = useRailWidth(activityLineRef);

  // THE DECISION LIVES IN @/lib/ask-activity-notice, NOT HERE. This route cannot be rendered by the
  // test setup, so logic left inline is logic nothing can assert. Precedence is the TUI's own
  // (transcriptStatusLine: reconnecting › disconnected › escalation › count › verb): reconnecting
  // returns null so the existing connection banner keeps its slot and the counter never claims a
  // liveness the plane cannot deliver.
  const activityLineInput: ActivityLineInput = {
    convId: activeConvId ?? "",
    turnInFlight,
    reconnecting,
    lastActivityMs: turnLastActivityAt ? turnLastActivityAt.toDate().getTime() : null,
    effectiveServerTimeMs,
    nowMs: liveNow,
    toolCalls: activityToolCalls,
    widthPx: activityLineWidth,
    cardPending: pendingFor(streams[activeConvId ?? ""]?.asks).length > 0,
  };
  const activityLine = activityLineFor(activityLineInput);
  const activityAnnouncement =
    activityLine === null ? "" : activityLineAnnouncement(activityLineInput);

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
    // AND IT KEEPS POLLING WHEN EVERYTHING IS SETTLED, slowly.
    //
    // It used to stop entirely (`: false`), which meant a change made in the OTHER client never arrived: the
    // operator — "if someone sets the mode in the GUI or TUI, it should not matter. It should change for both."
    // There is no conversation-changes stream to subscribe to (the Ask service has List/Get and the per-turn
    // streams, and nothing that broadcasts a mode change), so a slow poll is what makes the two clients agree.
    //
    // 5s at rest is the same cadence the TUI's rolling window already uses for its own list re-read, and it is
    // the bound on how long the mode control can disagree with the boundary the SERVER will enforce — which is
    // the case that matters, because a stale control turns a correct refusal into an inexplicable one.
    setListPollMs(anyStreaming || serverRunning ? 3000 : 5000);
  }, [conversations, anyStreaming]);
  const createConv = useCreateConversation();
  // The tenant's projects, for the scope dropdown. Every project is listed, including ones with no
  // conversations: the count then reads 0, which is how you know a scope is empty before clicking it.
  const { data: projects } = useListProjects();
  const deleteConv = useDeleteConversation();
  const updateTitle = useUpdateConversationTitle();
  const abortTurn = useAbortConversationTurn();
  const setMode = useSetConversationMode();
  const setConvModel = useSetConversationModel();
  const setFullsend = useSetConversationFullsend();
  const compactConv = useCompactConversation();
  const qc = useQueryClient();

  // askInFlight disables a card's actions while its reply is in the air, so one
  // decision cannot be sent twice (the server would report the second as
  // expired, which would mislabel an allow as an expiry).
  const [askInFlight, setAskInFlight] = useState<Record<string, boolean>>({});

  // handleAskDecision answers one pending consent ask. The outcome lands in the
  // TRANSCRIPT (the card settles in place) rather than only in the effect, and a
  // reply that did not apply is reported as expired with the server's reason —
  // never a silent nothing. On a transport error the card stays pending so the
  // operator can retry.
  const handleAskDecision = useCallback(
    async (
      convId: string,
      askId: string,
      choice: PermissionChoice,
      /** answer is the operator's words for a QUESTION ask; empty for a permission. */
      answer = "",
    ): Promise<void> => {
      if (askInFlight[askId]) return;
      setAskInFlight((prev) => ({ ...prev, [askId]: true }));
      try {
        const res = await askOrchiconClient.replyPermissionAsk({
          conversationId: convId,
          askId,
          choice,
          answer,
        });
        if (res.applied) {
          setStream(convId, (prev) => ({
            ...prev,
            asks: resolveAsk(prev.asks, askId, outcomeFromChoice(choice)),
          }));
          if (choice === PermissionChoice.ALLOW_SESSION) {
            // A session grant is now live: refresh the grants list (the
            // header's Grants disclosure) so it is visible without a reload.
            qc.invalidateQueries({ queryKey: askKeys.grants(convId) });
          }
        } else {
          setStream(convId, (prev) => ({
            ...prev,
            asks: resolveAsk(prev.asks, askId, {
              kind: "expired",
              detail: res.detail || undefined,
            }),
          }));
          toast.error(res.detail || "This ask is no longer open.", {
            title: "Permission",
          });
        }
      } catch (err: unknown) {
        // Leaves the ask PENDING: a failed reply is not a decision.
        toast.error(String(err instanceof Error ? err.message : err), {
          title: "Permission",
        });
      } finally {
        setAskInFlight((prev) => {
          const next = { ...prev };
          delete next[askId];
          return next;
        });
      }
    },
    [askInFlight, setStream, qc, toast],
  );

  // The effective model answering this conversation: the conversation's own
  // model_ref wins, then the tenant default, then the server's free fallback.
  // Surfacing this directly answers "which model is answering?" — the most
  // common cause of a stuck turn is a silently-fallen-back free model.
  const effectiveModel = useMemo(
    () =>
      activeConv?.modelRef ||
      settings?.defaultAskOrchiconModel ||
      "",
    [activeConv?.modelRef, settings?.defaultAskOrchiconModel],
  );
  // "No model configured" is its own state, and the composer says so rather than naming a model that
  // cannot dispatch. It is fixable in one click (the model chip), so it is a prompt, not an error.
  const isUsingFallbackModel =
    !activeConv?.modelRef && !settings?.defaultAskOrchiconModel;

  // The model the composer's chip reports: the open conversation's effective
  // model, or — on the hero — the pending choice for the conversation about to
  // be created.
  const askModel = activeConvId ? effectiveModel : pendingModel || effectiveModel;

  // Choosing a model in the composer's picker. With a conversation OPEN it
  // retargets THAT conversation (SetConversationModel — applies from the next
  // message). On the hero there is no conversation row yet, so the choice is
  // held for the create.
  const handleAskModelChange = useCallback(
    (ref: string) => {
      if (!ref) return;
      if (!activeConvId) {
        setPendingModel(ref);
        toast.success("Model set for the new conversation", { title: "Ask model" });
        return;
      }
      setConvModel.mutate(
        { id: activeConvId, modelRef: ref },
        {
          onSuccess: () =>
            toast.success("Model set for this conversation", { title: "Ask model" }),
          onError: () => toast.error("Failed to change the model", { title: "Error" }),
        },
      );
    },
    [activeConvId, setConvModel, toast],
  );

  // Sync local mode from active conversation when it loads.
  useEffect(() => {
    if (activeConv?.mode && (activeConv.mode as number) !== ConversationMode.UNSPECIFIED) {
      setLocalMode(activeConv.mode);
    }
  }, [activeConv?.mode]);

  // The reply (or error) is persisted under the acked assistant message id.
  // When it appears via polling AND the turn is no longer in flight, the turn
  // is over — clear the ACTIVE conversation's stream slot (other conversations
  // keep their own state, so a turn running while you browse elsewhere stays
  // intact). When the acked message came back as an ERROR (reply failed after
  // ack), the sent text is copied to the clipboard and the composer is
  // signalled to put it back in the box so the user never loses what they
  // typed.
  //
  // The server mirrors the running reply into the acked row as it is collected
  // (partial content), so a row under pendingReplyId can exist WHILE the turn
  // is still running — a refreshed / second-tab client watches it grow via the
  // 2s ListMessages poll. The turn registry is the terminal authority:
  // finalizing only when serverTurnInFlight is false keeps a partial row from
  // collapsing the re-attached streaming state (Stop button + banner) early.
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
    if (serverTurnInFlight) return;
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
  }, [messages, isStreaming, pendingReplyId, activeConvId, setStream, qc, toast, serverTurnInFlight]);

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
  // (the registry entry is removed before the reply row is written). The acked
  // assistant message row is mirrored as the reply is collected, so a row's
  // presence does NOT mean the turn is done — re-arm whenever the server
  // reports the turn in flight with a pending id, and let the completion
  // effect (which waits for turn_in_flight to clear) resolve the slot once the
  // final reply lands.
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
    if (!pendingId) return;
    setStream(activeConvId, (prev) => {
      if (prev.isStreaming) return prev;
      return {
        ...prev,
        isStreaming: true,
        isThinking: true,
        reconnecting: true,
        turnProgressing:
          server?.turnProgressing ?? activeConv?.turnProgressing ?? false,
        pendingReplyId: pendingId || prev.pendingReplyId,
        items: [],
      };
    });
    // Mark the slot as RESTORED (we did not dispatch this turn). The watch
    // re-dial lives in its own effect below, because this one cannot reference
    // runWatch (a later const — naming it here would be a TDZ error). Without
    // the re-dial a PENDING CONSENT ASK never arrives on a restored turn:
    // WatchTurnStream replays open asks to a LATE subscriber, and a re-attached
    // tab is exactly that.
    restoredWatchRef.current[activeConvId] = true;
  }, [activeConvId, activeConv, conversations, streams, setStream]);

  // Keep the slot's turnProgressing fresh while a server turn is re-attached:
  // it is set once at re-arm, but the server recomputes it on every
  // conversations poll (3s) against the tenant's stall window. Syncing it lets
  // the banner flip from "still working" to "turn stalled" without a manual
  // refresh, and keeps the "still working" claim honest.
  useEffect(() => {
    if (!activeConvId) return;
    const server = conversations?.find((c) => c.id === activeConvId);
    const sp = server?.turnProgressing ?? activeConv?.turnProgressing;
    if (sp === undefined) return;
    setStream(activeConvId, (prev) => {
      if (!prev.isStreaming || prev.turnProgressing === sp) return prev;
      return { ...prev, turnProgressing: sp };
    });
  }, [activeConvId, conversations, activeConv, setStream]);

  // ESCAPE MEANS DENY while a consent ask is pending — never a silent dismissal,
  // which would leave the turn waiting with no way back. The card's own handler
  // runs first (the listener is on the card) and calls preventDefault when it
  // consumed the key (collapsing the question card's free-text row), so this
  // document-level listener skips an already-handled Escape and one press never
  // denies twice.
  const oldestPendingAsk = activeConvId
    ? pendingFor(streams[activeConvId]?.asks)[0]
    : undefined;
  useEffect(() => {
    if (!activeConvId || !oldestPendingAsk) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || e.defaultPrevented) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      void handleAskDecision(
        activeConvId,
        oldestPendingAsk.ask.askId,
        PermissionChoice.DENY,
      );
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [activeConvId, oldestPendingAsk, handleAskDecision]);

  const handleNewChat = useCallback(() => {
    setActiveConvId(null);
    // A NEW CONVERSATION STARTS AT THE DEFAULT MODE, the same rule the TUI applies.
    //
    // The operator: "When someone creates a new conversation, it should always default back to brainstorm unless
    // they do /mode again." Without this the mode LEAKED FORWARD: the sync effect only writes localMode when
    // there IS an active conversation, so after New Chat the previous conversation's mode stayed in state and
    // was handed to the next conversation at creation. A mode belongs to the conversation that was set on.
    setLocalMode(ConversationMode.BRAINSTORM);
    // AND THE PROJECT PICK IS CLEARED WITH IT, for the same reason and by the same rule: a workspace
    // belongs to the conversation it was chosen for. Leaving it set would file the NEXT new chat into
    // a workspace the operator picked for the previous one — silent, and the exact class of leak the
    // mode comment above records.
    setComposerProjectId(null);
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

  // Re-dial a dropped socket on an ACKED turn: opens WatchTurnStream
  // against the in-flight turn's broadcast hub WITHOUT dispatching, so
  // live TextChunk/Reasoning chunks resume appending to the SAME bubbles.
  // Guarded by the dispatch generation: a stale generation (a newer
  // interject owns the slot) never touches the state, and completion
  // still resolves via the ListMessages + turnInFlight poll as today.
  // No duplicate turns (watch dispatches nothing), no dispatchGen race.
  const runWatch = useCallback(
    async (convId: string, replyId: string, gen: number): Promise<void> => {
      let watch;
      try {
        watch = askOrchiconClient.watchTurnStream({
          conversationId: convId,
          assistantMessageId: replyId,
        });
      } catch {
        return; // poll resolves completion; a failed watch is not fatal
      }
      liveStreamRef.current[convId] = true;
      try {
        for await (const chunk of watch) {
          if (dispatchGenRef.current[convId] !== gen) return;
          if (chunk.event.case === "textChunk") {
            const content = chunk.event.value.content;
            if (content) {
              const seq = (chunkSeqRef.current[convId] ?? 0) + 1;
              chunkSeqRef.current[convId] = seq;
              const key = nextChunkKey("st", seq);
              setStream(convId, (prev) => ({
                ...prev,
                reconnecting: false,
                items: [
                  ...prev.items,
                  { kind: "text", text: content, at: Date.now(), key, phase: "p-0" },
                ],
              }));
            }
          } else if (chunk.event.case === "reasoning") {
            const content = chunk.event.value.content;
            if (content) {
              const seq = (chunkSeqRef.current[convId] ?? 0) + 1;
              chunkSeqRef.current[convId] = seq;
              const key = nextChunkKey("sr", seq);
              setStream(convId, (prev) => ({
                ...prev,
                reconnecting: false,
                items: [
                  ...prev.items,
                  { kind: "reasoning", text: content, at: Date.now(), key, phase: "p-0" },
                ],
              }));
            }
          } else if (chunk.event.case === "heartbeat") {
            // Server keepalive. Beyond clearing the reconnecting banner, this is where the ROTATING
            // ACTIVITY VERB gets its clock: server_time_unix_ms is already on the wire (the proto
            // documents it as the client's means to measure socket age/skew) and this client used to
            // discard it. Number(...) is required — the generated field is a bigint (protoInt64.zero),
            // and `bigint % number` throws.
            const stamp = Number(chunk.event.value.serverTimeUnixMs);
            setStream(convId, (prev) => ({
              ...prev,
              ...(stamp > 0 ? { serverTimeMs: stamp, serverTimeRecvAt: Date.now() } : {}),
              ...(prev.reconnecting ? { reconnecting: false } : {}),
            }));
          } else if (chunk.event.case === "permissionAsk") {
            // The ask arm the GUI never read: a pending consent ask arrives on
            // the SAME turn stream (never a second polling loop).
            applyAsk(convId, chunk.event.value);
          } else if (chunk.event.case === "permissionAskResolved") {
            // SOMEONE ELSE DECIDED — on the WATCH socket too, not just the dispatch
            // socket: a watcher that re-attached mid-turn is exactly the client most
            // likely to be holding a card it did not answer. See the dispatch arm.
            const r = chunk.event.value;
            setStream(convId, (prev) => ({
              ...prev,
              asks: resolveAsk(prev.asks, r.askId, outcomeFromWire(r.outcome)),
            }));
          } else if (chunk.event.case === "error") {
            return; // poll resolves the failure rendering
          }
        }
      } catch {
        // Watch socket dropped again (or the hub closed on finalize) —
        // the slot stays reconnecting and the poll resolves completion.
      } finally {
        liveStreamRef.current[convId] = false;
      }
    },
    [setStream, applyAsk],
  );

  // Re-dial the watch for a slot we RESTORED rather than dispatched (see the
  // re-attach effect above). It lives here, after runWatch, because the
  // re-attach effect cannot reference runWatch at all. The generation is pinned
  // BEFORE dialling: on a fresh page load the map is empty, and the watch's own
  // `dispatchGenRef !== gen` guard would otherwise reject every chunk it
  // delivered — including a replayed consent ask.
  useEffect(() => {
    if (!activeConvId) return;
    if (!restoredWatchRef.current[activeConvId]) return;
    const slot = streams[activeConvId];
    if (!slot?.reconnecting || !slot.pendingReplyId) return;
    restoredWatchRef.current[activeConvId] = false;
    const gen = dispatchGenRef.current[activeConvId] ?? 0;
    dispatchGenRef.current[activeConvId] = gen;
    void runWatch(activeConvId, slot.pendingReplyId, gen);
  }, [activeConvId, streams, runWatch]);

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
      // owns the slot) never touches the state. On an ACKED turn the live
      // stream is re-dialled via WatchTurnStream (same gen guard) so chunks
      // resume appending instead of freezing until the poll resolves.
      const fail = (err?: unknown) => {
        let watchReplyId: string | null = null;
        setStream(convId, (prev) => {
          if (dispatchGenRef.current[convId] !== gen) {
            return prev;
          }
          if (prev.pendingReplyId) {
            watchReplyId = prev.pendingReplyId;
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
        if (watchReplyId) {
          void runWatch(convId, watchReplyId, gen);
        }
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
            // THE RAIL IS REFRESHED AT ONCE, so the row marks the conversation it just started. The ack is
            // the FIRST moment the turn provably exists server-side, and the running set already includes
            // this conversation from the LOCAL half (streams[convId].isStreaming, set by sendStreaming before
            // the call) — so this invalidate makes the sidebar row catch up without waiting for the 3s/5s list
            // poll. That is the "a send is reflected promptly" half of "the rail's running marker misses
            // active turns": before it, a just-sent turn's row could lag by seconds.
            qc.invalidateQueries({ queryKey: askKeys.conversations });
          } else if (chunk.event.case === "textChunk") {
            const content = chunk.event.value.content;
            if (content) {
              const seq = (chunkSeqRef.current[convId] ?? 0) + 1;
              chunkSeqRef.current[convId] = seq;
              const key = nextChunkKey("st", seq);
              setStream(convId, (prev) => ({
                ...prev,
                items: [
                  ...prev.items,
                  {
                    kind: "text",
                    text: content,
                    at: Date.now(),
                    key,
                    phase: "p-0",
                  },
                ],
              }));
            }
          } else if (chunk.event.case === "reasoning") {
            const content = chunk.event.value.content;
            if (content) {
              const seq = (chunkSeqRef.current[convId] ?? 0) + 1;
              chunkSeqRef.current[convId] = seq;
              const key = nextChunkKey("sr", seq);
              setStream(convId, (prev) => ({
                ...prev,
                items: [
                  ...prev.items,
                  {
                    kind: "reasoning",
                    text: content,
                    at: Date.now(),
                    key,
                    phase: "p-0",
                  },
                ],
              }));
            }
          } else if (chunk.event.case === "heartbeat") {
            // Server keepalive. This is the WATCH (re-dial) arm, and it must feed the verb rotation's
            // clock too — a watcher that re-attached mid-turn is the client least likely to hold a
            // stamp. Same Number(...) coercion as the dispatch arm: the field is a bigint on the wire.
            const stamp = Number(chunk.event.value.serverTimeUnixMs);
            setStream(convId, (prev) => ({
              ...prev,
              ...(stamp > 0 ? { serverTimeMs: stamp, serverTimeRecvAt: Date.now() } : {}),
              ...(prev.reconnecting ? { reconnecting: false } : {}),
            }));
          } else if (chunk.event.case === "permissionAsk") {
            applyAsk(convId, chunk.event.value);
          } else if (chunk.event.case === "permissionAskResolved") {
            // SOMEONE ELSE DECIDED. The ask reaches every watcher of the turn, but only
            // the answering client cleared its own copy — so a decision made in the TUI
            // left this client showing a live-looking, inert card, and it cannot be
            // inferred: a permission ask has no durable per-ask row to reconcile
            // against. The collector publishes the outcome precisely so this can settle.
            const r = chunk.event.value;
            setStream(convId, (prev) => ({
              ...prev,
              asks: resolveAsk(prev.asks, r.askId, outcomeFromWire(r.outcome)),
            }));
          } else if (chunk.event.case === "error") {
            toast.error(chunk.event.value.message);
            fail();
            return false;
          }
        }
        if (!acked) {
          fail();
        }
        // ONE FINAL DURABLE FETCH, after the turn ends.
        //
        // The turn's tool calls land on the message row at FINALIZE — after the
        // last chunk — and this stream's poll stops the moment streaming ends
        // (`transcriptPollMs(turnInFlight)`). So without this the newest durable
        // content can go unfetched, and a recorded ask_user call
        // does not appear in the GUI at all. The operator: "it never popped up in
        // the GUI but was in the TUI" — the TUI happened to re-read, the GUI had
        // no reason to. The same race hid the tool calls of a just-finished reply
        // until the page was reloaded.
        qc.invalidateQueries({ queryKey: askKeys.messages(convId) });
      } catch (err: unknown) {
        fail(err);
      } finally {
        liveStreamRef.current[convId] = false;
      }
      return acked;
    },
    [toast, setStream, runWatch, applyAsk, qc],
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

  // An interjection: sent while a turn is already streaming. The server ADDS the
  // message to that turn — it persists it (so it shows up in the transcript like
  // any other user message) and injects it into the live session, so the model
  // treats it as more context for the work in hand. Nothing is aborted.
  //
  // The ack still carries an assistant message id: the RUNNING turn's, because the
  // reply this message just joined is the one that continues. The stream slot
  // therefore keeps pointing at the message the operator was already watching.
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

  // handleCompactConversation runs the /compact composer command. The server owns
  // the policy (it may decline with a reason), so the outcome is passed back
  // STRUCTURED rather than flattened to a string: `compacted` lets the caller
  // render a decline as information instead of a success.
  const handleCompactConversation = useCallback(
    async (conversationId: string): Promise<{ detail: string; compacted: boolean }> => {
      const res = await compactConv.mutateAsync(conversationId);
      // The server never estimates: a non-zero size is a real measurement, so a
      // zero is omitted rather than shown as "0 tokens".
      const before = Number(res.contextTokensBefore ?? 0);
      const after = Number(res.contextTokensAfter ?? 0);
      const size = before > 0 ? ` (${before} → ${after} tokens)` : "";
      const verdict = res.detail || (res.compacted ? "conversation compacted" : "nothing to compact");
      return { detail: `${verdict}${size}`, compacted: res.compacted };
    },
    [compactConv],
  );

  const handleSendMessage = useCallback(
    async (text: string, attachments?: AttachmentInput[]): Promise<boolean> => {
      if (!text.trim() || !activeConvId) return false;
      if (isStreaming || serverTurnInFlight) {
        // Send while streaming = interject: the server ADDS this message to the
        // reply in flight (it persists it and injects it into the live session,
        // so the model picks it up as more context) without aborting anything.
        // Never the "another reply already processing" dead-end.
        // serverTurnInFlight covers the case where THIS browser's slot is
        // idle but the turn is running server-side (started here before a
        // refresh, or in another tab/device): the poll may not have re-armed
        // the slot yet, but a send must still interject, not double-start.
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
    [activeConvId, isStreaming, serverTurnInFlight, sendStreaming, interjectStreaming],
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

  // The FULLSEND toggle. NO OPTIMISTIC FLIP HERE, unlike the mode above.
  //
  // The mode can flip locally and roll back because a wrong mode is merely a wrong persona for
  // one turn. This is the permission gate: rendering it "on" before the server has confirmed
  // would tell the operator the gate is down while it is still up, and they would proceed on
  // that. So the control re-renders from Conversation.fullsend (which the mutation's
  // invalidation refreshes) and this handler only reports failure.
  const handleFullsendChange = useCallback(
    (next: boolean) => {
      if (!activeConvId) {
        // There is deliberately no pending form: a bypass armed for a conversation the operator
        // has not opened is one they did not knowingly turn on (the leak the conversation mode
        // had, and had fixed the same way). The control is not rendered without a conversation,
        // so this is a guard against a future caller rather than a reachable path.
        toast.error("Open a conversation first — fullsend applies to one conversation", {
          title: "Fullsend",
        });
        return;
      }
      setFullsend.mutate(
        { id: activeConvId, enabled: next },
        {
          onError: () =>
            toast.error("Failed to change fullsend — permissions are unchanged", {
              title: "Error",
            }),
        },
      );
    },
    [activeConvId, setFullsend, toast],
  );

  // DnD sensors
  const dndSensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 5 } }),
  );

  // Click-suppression standard (mirrors CategoryDndContext): after a dnd-kit
  // drag the browser dispatches a synthetic click whose propagation path is
  // only [target -> document], so a document capture-phase listener is the
  // only reliable place to consume it — otherwise the release click lands on
  // the row/folder under the pointer and selects/toggles it mid-drop.
  const suppressClickRef = useRef(false);
  useEffect(() => {
    const onCaptureClick = (e: MouseEvent) => {
      if (suppressClickRef.current) {
        e.preventDefault();
        e.stopPropagation();
        suppressClickRef.current = false;
      }
    };
    document.addEventListener("click", onCaptureClick, true);
    return () => document.removeEventListener("click", onCaptureClick, true);
  }, []);
  const armClickSuppressionBackstop = useCallback(() => {
    window.setTimeout(() => {
      suppressClickRef.current = false;
    }, 300);
  }, []);

  // Handle drag start
  const handleDragStart = useCallback(
    (event: { active: { id: string | number } }) => {
      suppressClickRef.current = true;
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
      // Backstop: keep the flag set long enough that the drag's post-mouseup
      // click lands inside it, then clear (mirrors CategoryDndContext).
      armClickSuppressionBackstop();
      const { active, over } = event;
      setActiveDragId(null);
      setOverFolderId(null);
      if (!over) return;
      const convId = String(active.id);
      const targetId = String(over.id);
      // Drop on "uncategorized" = remove the category assignment (the project is untouched: a project is the
      // WORKSPACE and a category is a label, so this drag changes one and not the other).
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
    [convPrefs, armClickSuppressionBackstop],
  );

  // A cancelled drag (e.g. Escape) still ends with a possible click under the
  // pointer — arm the same backstop so a stray click is suppressed and the
  // flag clears shortly after (mirrors CategoryDndContext).
  const handleDragCancel = useCallback(() => {
    armClickSuppressionBackstop();
    setActiveDragId(null);
    setOverFolderId(null);
  }, [armClickSuppressionBackstop]);

  // THE PROJECT SCOPE. A project is a WORKSPACE: the sidebar shows one at a time, and the categories reappear
  // inside it holding only that scope's conversations. Defaults to All projects so nothing that predates the
  // project column — which defaults to empty — is hidden on first load.
  const [projectScope, setProjectScope] = useState<string>(ALL_PROJECTS);
  const projectScopeOptions = useMemo(() => scopeOptions(projects, conversations), [projects, conversations]);
  const scopedConversations = useMemo(
    () => filterConversationsByScope(conversations, projectScope),
    [conversations, projectScope],
  );
  // The selected project when the scope names a REAL project, so a new conversation can be created in it. An
  // unscoped view (All projects) or the No-project scope creates unassigned, which is what it always did.
  const scopeProjectId = useMemo(() => {
    if (projectScope === ALL_PROJECTS || projectScope === "") return "";
    return projects?.some((p) => p.id === projectScope) ? projectScope : "";
  }, [projectScope, projects]);

  // setConvProject moves a conversation into a project — the row's "move to project" action. The sidebar is
  // SCOPED by project rather than showing one folder per project, so this is an explicit per-conversation
  // action rather than a drop target.
  const setConvProject = useSetConversationProject();

  // renderMoveControl builds a conversation row's "move to project" control. A render prop rather than a plain
  // callback, because the control is a popover with its own open state — and constructing it here is what lets
  // the row reuse the SAME control as the sidebar header, so one place decides the option list, the archived
  // marker and the unknown-project fallback. "All projects" is filtered out: it is a way to LOOK at the list,
  // not a place to put a conversation.
  //
  // This is the sidebar's answer to the drag-to-project gesture the first attempt had. With projects as a
  // SCOPE there is no project folder to drop onto, and moving a chat between workspaces still has to be
  // possible from the row itself.
  const renderMoveControl = useCallback(
    (convId: string, currentProjectId: string) => (
      <ProjectScopeSelect
        compact
        label="Move to project"
        options={projectScopeOptions.filter((o) => o.value !== ALL_PROJECTS)}
        value={currentProjectId}
        onChange={(next) => {
          setConvProject.mutate(
            { id: convId, projectId: next },
            { onError: () => toast.error("Failed to move conversation", { title: "Error" }) },
          );
        }}
      />
    ),
    [projectScopeOptions, setConvProject, toast],
  );

  // Sync the composer's project from the open conversation, exactly as the mode above is synced: an
  // explicit choice is cleared when the open conversation changes, so the pill always shows the row it
  // would actually write to rather than a stale pick from a previous chat.
  useEffect(() => {
    setComposerProjectId(null);
  }, [activeConvId]);

  // THE PROJECT THE COMPOSER SHOWS AND USES. An explicit pick wins; otherwise the rail's scope, so a
  // conversation started while the sidebar is scoped to a workspace is created in it without the
  // operator having to also touch the pill. "All projects" and the unassigned scope both yield "", so
  // an unscoped view still creates unassigned, and a "not chosen" pill cannot silently invent a home.
  // The DECISION lives in lib/conversationProjects, as a pure function, because this route cannot be
  // rendered by the test setup — logic left inline here is logic nothing can assert. See its own tests.
  const effectiveProjectId = effectiveComposerProject(
    composerProjectId,
    scopeProjectId,
    projects,
  );

  // The option list for the composer pill: the same choices the rail's move control offers, minus
  // "All projects" (a way to LOOK at the list, not a place to put a conversation).
  const composerProjectOptions = useMemo(
    () => composerProjectChoices(projectScopeOptions),
    [projectScopeOptions],
  );

  // handleComposerProjectChange writes the project the way the OTHER project control on this page
  // does. See its sibling above (setConvProject): the SAME rpc, so the two clients — and the two
  // controls in this one — cannot disagree about what "belongs to a project" means.
  //
  // WITH NO OPEN CONVERSATION THERE IS NOTHING TO WRITE, so the choice is simply held for the create
  // that follows; with one open, the choice is applied immediately and the list is invalidated by the
  // mutation so the rail's scope and counts follow it.
  const handleComposerProjectChange = useCallback(
    (next: string) => {
      setComposerProjectId(next || null);
      if (activeConvId) {
        setConvProject.mutate(
          { id: activeConvId, projectId: next },
          { onError: () => toast.error("Failed to move conversation", { title: "Error" }) },
        );
      }
    },
    [activeConvId, setConvProject, toast],
  );

  // Build categorized conversation groups — WITHIN THE PROJECT SCOPE.
  const categorizedConversations = useMemo(() => {
    if (!conversations) return { categorized: new Map<string, string[]>(), uncategorized: [] as string[] };
    return getItemsForCategory(convPrefs.state, scopedConversations.map((c) => c.id));
  }, [scopedConversations, convPrefs.state]);

  // THE FOLDERS THIS SCOPE ACTUALLY USES.
  //
  // The operator: "In the GUI, folders are visible no matter what project you are on. This is wrong. You should
  // only see the categories/folders of the currently selected project." The sidebar rendered every category the
  // tenant has, so five folders showed on a screen scoped to "No project" — none of them empty, all of them
  // holding conversations that live in a different project.
  //
  // The RULE lives in lib/conversationProjects as a pure function, because this route component cannot be
  // rendered by the test setup — logic left inline here is logic nothing can assert. This is only the wiring:
  // the SCOPED grouping in, the folders to draw out. ALL_PROJECTS deliberately keeps every folder, so one you
  // just created is visible to drag into (creating a folder assigns no conversations).
  const visibleCategories = useMemo(
    () => categoriesForScope(convPrefs.state.categories, categorizedConversations.categorized, projectScope),
    [convPrefs.state.categories, categorizedConversations, projectScope],
  );

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

  // (groupedStream is declared ABOVE, beside `streamItems`: the transcript builder reads it.)

  // Merge durable messages with streaming items for display.
  // Durable messages are the source of truth; streaming items fill in the gap.
  const displayMessages = useMemo(() => {
    // The SAME supersession rule as the transcript above: with live items drawing the running
    // reply, the mirrored row must not be merged in beside them or the reply is drawn twice. This
    // is the scroll dep and the empty-check input, so it must agree with `transcriptBlocks`.
    const durable = dropSupersededMirror(
      messages ?? [],
      pendingReplyId,
      isStreaming,
      hasLiveTextItems(groupedStream),
    );
    if (!isStreaming || groupedStream.length === 0) return durable;
    // Build a simple merged list: durable messages + streaming items.
    return [...durable, ...groupedStream] as ChatMessage[];
  }, [messages, isStreaming, groupedStream, pendingReplyId]);

  // (There was a `lastMessageId` here, used to decide whether a clarifying-question
  // card was still interactive by asking "does a later message exist?". That was right
  // while answering SENT the choice as the next user message, and became permanently
  // false once ask_user was made BLOCKING — the answer arrives as the tool RESULT and
  // nothing follows. The card now reads its own result instead, so the variable is
  // gone rather than left as a second, wrong source of truth.)

  return (
    <div ref={diffRowRef} className="flex flex-1 min-h-0 h-full items-stretch gap-0 min-w-0 overflow-hidden">
      {/* Left diff rail — slide-out file-edit + diff side-by-side view.
          First flex child; the chat column is flex-1 min-w-0 so it keeps
          width as the rail opens. Distinct from the right conversation panel. */}
      <DiffSidebar
        open={diffOpen}
        onClose={() => setDiffOpen(false)}
        ownerKind="ask_conversation"
        ownerId={activeConvId ?? ""}
        isLive={isStreaming}
        title="Diff"
        tab={diffTab}
        onTabChange={setDiffTab}
        selectedPath={diffPath}
        onSelectPath={setDiffPath}
        containerRef={diffRowRef}
        width={railWidth}
        onWidthChange={setRailWidth}
      />
      {/* Main chat area — centered column */}
      <div className="flex flex-1 flex-col min-h-0 min-w-0">
        {!activeConvId ? (
          /* --- Greeting state: centered + header on mobile --- */
          <div className="flex flex-1 flex-col min-h-0">
            {/* Greeting header — mobile only, provides conversations affordance */}
            <div className="flex items-center justify-between border-b px-4 sm:px-6 py-3 shrink-0 gap-2 lg:hidden">
              <span className="text-sm font-medium">Ask Orchicon</span>
              <button
                ref={mobileTriggerRef}
                onClick={openMobileSheet}
                aria-label="Conversations"
                data-testid="ask-conversation-sheet-trigger-greeting"
                className="flex h-11 w-11 min-h-[44px] min-w-[44px] items-center justify-center rounded-xl glass-panel border border-black/10 dark:border-white/10 text-muted-foreground hover:text-foreground lg:hidden shrink-0"
              >
                <PanelRight aria-hidden="true" className="h-5 w-5" />
              </button>
            </div>
            <div className="flex flex-1 items-center justify-center px-4 pb-2">
            <div className="w-full max-w-2xl space-y-8">
              <div className="text-center space-y-4">
                <div className="flex justify-center">
                  <span className="inline-flex items-center gap-2 glass-panel rounded-full px-3.5 py-1.5 text-xs uppercase tracking-wider text-cyan-700 dark:text-cyan-300">
                    <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-cyan-400 animate-pulse" />
                    Intelligent Orchestration
                  </span>
                </div>
                <h1 className="text-4xl sm:text-5xl lg:text-6xl font-extrabold tracking-tight">
                  <span className="text-foreground">Orchestrate </span>
                  <span className="bg-gradient-to-r from-cyan-400 via-sky-300 to-indigo-300 bg-clip-text text-transparent">with intention</span>
                </h1>
                <p className="text-lg text-slate-600 dark:text-slate-400 max-w-2xl mx-auto font-light leading-relaxed text-balance">
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
                      // The model chosen on the hero, if any — this is what makes
                      // the chip work before a conversation exists.
                      modelRef: pendingModel,
                      // The project chosen on the composer pill, falling back to the sidebar's scope — so a
                      // new conversation is filed in the workspace the operator picked, or the one they are
                      // looking at. An unscoped, unpicked view creates unassigned, exactly as before.
                      projectId: effectiveProjectId,
                    });
                    if (conv?.id) {
                      // Consumed by THIS conversation; a later new chat starts
                      // from the tenant default again rather than silently
                      // inheriting a one-off choice.
                      setPendingModel("");
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
                projectOptions={composerProjectOptions}
                projectId={effectiveProjectId}
                onProjectChange={handleComposerProjectChange}
                convId={activeConvId}
                modelRef={askModel}
                onModelChange={handleAskModelChange}
              />
            </div>
            </div>
          </div>
        ) : (
          /* --- Active conversation: scroll + pinned input --- */
          <div className="flex flex-1 flex-col min-h-0">
            {/* Chat header */}
            <div className="flex items-center justify-between border-b px-4 sm:px-6 py-3 shrink-0 gap-2">
              <div className="flex items-center gap-2 min-w-0">
                <button
                  ref={mobileTriggerRef}
                  onClick={openMobileSheet}
                  aria-label="Conversations"
                  data-testid="ask-conversation-sheet-trigger"
                  className="flex h-11 w-11 min-h-[44px] min-w-[44px] items-center justify-center rounded-xl glass-panel border border-black/10 dark:border-white/10 text-muted-foreground hover:text-foreground lg:hidden shrink-0"
                >
                  <PanelRight aria-hidden="true" className="h-5 w-5" />
                </button>
                <button
                  type="button"
                  onClick={toggleDiffSidebar}
                  aria-label="Toggle diff sidebar"
                  aria-expanded={diffOpen}
                  data-testid="ask-diff-sidebar-trigger"
                  className="flex h-11 w-11 min-h-[44px] min-w-[44px] items-center justify-center rounded-xl glass-panel border border-black/10 dark:border-white/10 text-muted-foreground hover:text-foreground shrink-0"
                >
                  {diffOpen ? (
                    <PanelLeftClose aria-hidden="true" className="h-5 w-5" />
                  ) : (
                    <PanelLeft aria-hidden="true" className="h-5 w-5" />
                  )}
                </button>
                <h2 className="text-sm font-medium truncate">
                  {activeConv?.title || "Ask Orchicon"}
                </h2>
                {/* THE MODE THIS CONVERSATION IS ACTUALLY IN.
                    This pill read "Brainstorm" on every conversation whatever its real mode — one of three
                    places carrying the same hardcoded single-mode assumption (lib/conversationModes.ts makes
                    the list one thing now). It is worth fixing rather than leaving as a cosmetic oddity: the
                    mode is ENFORCED by the platform, so in Iteration the planning tools are refused and in
                    Brainstorm the work tools are, which makes a pill that names the wrong mode a statement
                    about what this conversation can DO, not just a mislabel. */}
                <span
                  className="shrink-0 inline-flex items-center gap-1 rounded-full bg-violet-100 px-2 py-0.5 text-[10px] font-medium text-violet-700 dark:bg-violet-900/30 dark:text-violet-300"
                  title={conversationModeMeta(localMode)?.blurb}
                >
                  <ModeIcon mode={localMode} className="h-2.5 w-2.5" />
                  {conversationModeLabel(localMode)}
                </span>
                {/* The model answering this conversation.
                    "No model configured" is its own state, and it is NOT a model name: there is no
                    last-resort model any more, because every candidate could only fail (a legacy ref
                    inferred the opencode adapter, and the free tier refuses any client but opencode
                    itself). Saying so is a prompt the operator can act on; naming a model that cannot
                    dispatch is an error they cannot. */}
                <span
                  className={cn(
                    "shrink-0 max-w-[220px] truncate inline-flex items-center rounded-md px-2 py-0.5 font-mono text-[10px]",
                    isUsingFallbackModel
                      ? "bg-amber-100 text-amber-800 dark:bg-amber-900/30 dark:text-amber-300"
                      : "bg-muted text-muted-foreground",
                  )}
                  title={
                    isUsingFallbackModel
                      ? "No Ask Orchicon model is configured — pick one here, or set a default in Settings → Defaults."
                      : "The model answering this conversation"
                  }
                >
                  {isUsingFallbackModel ? NO_MODEL_CONFIGURED_LABEL : effectiveModel}
                </span>
              </div>
              <div className="flex items-center gap-2 shrink-0">
                {/* This conversation's active session grants, with revoke.
                    Small and discoverable rather than prominent: it is a short
                    list, and most of the time it is empty. */}
                <SessionGrants conversationId={activeConvId ?? ""} />
                {/* This conversation's OWN MCP servers + skill files, the exact
                    sibling of the Grants disclosure above. One MCP surface for
                    every scope (MCPServersPanel). */}
                <ConversationScopeDisclosure conversationId={activeConvId ?? ""} />
                <Button variant="ghost" size="sm" onClick={handleNewChat}>
                  <Plus aria-hidden="true" className="h-4 w-4" />
                </Button>
              </div>
            </div>

            {/* Fallback-model warning: no Ask Orchicon model is configured,
                so the server is answering with the free fallback model. That
                model is rate-limited and is the #1 cause of a stuck turn —
                surfacing it turns a confusing "Orchicon is thinking" into an
                actionable "your model config is missing".
                (There is no fallback model any more: see NO_MODEL_CONFIGURED_LABEL.) */}
            {isUsingFallbackModel && (
              <div className="flex items-start gap-2 border-b border-amber-300/40 bg-amber-50/60 px-6 py-2 text-xs text-amber-800 dark:border-amber-900/40 dark:bg-amber-950/20 dark:text-amber-300">
                <RefreshCw aria-hidden="true" className="mt-0.5 h-3 w-3 shrink-0" />
                <span className="min-w-0 [overflow-wrap:anywhere]">
                  No Ask Orchicon model is configured.{" "}
                  <button
                    type="button"
                    // The chip is the control; pointing at it is not enough when the operator is
                    // reading a banner at the other end of the composer. This opens the same picker
                    // by clicking the chip for them.
                    onClick={() =>
                      document
                        .querySelector<HTMLButtonElement>('[data-testid="ask-model-chip"]')
                        ?.click()
                    }
                    className="font-medium underline underline-offset-2 hover:no-underline"
                  >
                    Choose a model
                  </button>{" "}
                  from the chip beside the message box, or set a default in Settings
                  → Defaults → “Default Ask Orchicon model”. A message sent without
                  one is refused — the native “orchicon” adapter needs nothing
                  installed.
                </span>
              </div>
            )}

            {/* Transcript search: filter the conversation's messages and step the
                matches. The status/steppers stay hidden until a query is typed
                (see TranscriptSearchBar). */}
            <TranscriptSearchBar
              query={searchQuery}
              onQueryChange={(q) => {
                setSearchQuery(q);
                setActiveMatch(0);
              }}
              matchCount={searchMatchCount}
              total={transcriptSlots.length}
              inputRef={searchInputRef}
              onNext={() =>
                setActiveMatch((i) => (searchMatchCount ? (i + 1) % searchMatchCount : 0))
              }
              onPrev={() =>
                setActiveMatch((i) =>
                  searchMatchCount ? (i - 1 + searchMatchCount) % searchMatchCount : 0,
                )
              }
              onClear={() => {
                setSearchQuery("");
                setActiveMatch(0);
              }}
              // Escape clears the query and hands the keyboard back to the composer,
              // so abandoning a search puts the cursor where the operator types next.
              onEscape={() => {
                setSearchQuery("");
                setActiveMatch(0);
                composerInputRef.current?.focus();
              }}
            />

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

                {/* THE TRANSCRIPT, WITH THE CONSENT CARDS IN IT.
                    A card used to be a separate list rendered AFTER every
                    message, so a settled card sat pinned to the bottom of the
                    conversation forever. The operator: "the permission blocks in
                    the GUI are still remaining at the bottom at the end of a
                    turn which makes no sense. They should be in the conversation
                    and move up just like any other conversation block."

                    So the messages and the asks are merged into ONE time-ordered
                    sequence and rendered together: a card sits between the
                    messages it happened between, and moves up as the conversation
                    grows, exactly like anything else. */}
                {/* NO MATCHES: an explicit empty state, so a filter that finds
                    nothing cannot read as a broken or empty conversation. */}
                {searchActive && searchMatchCount === 0 && (
                  <div
                    data-testid="transcript-search-empty"
                    className="py-8 text-center text-sm text-muted-foreground"
                  >
                    No matches
                  </div>
                )}
                {(!searchActive || searchMatchCount > 0) &&
                  visibleSlots.map((slot, matchIdx) => {
                    const block = slot.block as TranscriptBlock;
                    const key = slot.isAsk
                      ? `ask:${(slot.block as AskItem).key}`
                      : block.kind === "optimistic"
                        ? "optimistic"
                        : `msg:${(slot.block as { kind: "message"; message: { id: string } }).message.id}`;
                    return (
                      // The wrapper carries the match ref/geometry so next/prev can
                      // scroll to THIS match (and it keeps the bubbles' own layout,
                      // which the space-y-4 parent provides per direct child).
                      <div
                        key={key}
                        ref={(el) => {
                          searchMatchRefs.current[matchIdx] = el;
                        }}
                        data-transcript-match={matchIdx}
                      >
                        {slot.isAsk ? (
                          <ConsentAskCard
                            ask={(slot.block as AskItem).ask}
                            outcome={(slot.block as AskItem).outcome}
                            busy={!!askInFlight[(slot.block as AskItem).ask.askId]}
                            onDecide={(choice) =>
                              void handleAskDecision(
                                activeConvId!,
                                (slot.block as AskItem).ask.askId,
                                choice,
                              )
                            }
                            // A QUESTION's answer is CONTENT (see ConsentAskCard): it becomes
                            // the ask_user tool result and resumes the PAUSED turn, so it goes
                            // as `answer` with no permission choice.
                            onAnswer={(text) =>
                              void handleAskDecision(
                                activeConvId!,
                                (slot.block as AskItem).ask.askId,
                                PermissionChoice.UNSPECIFIED,
                                text,
                              )
                            }
                            onEscape={() =>
                              void handleAskDecision(
                                activeConvId!,
                                (slot.block as AskItem).ask.askId,
                                PermissionChoice.DENY,
                              )
                            }
                          />
                        ) : block.kind === "optimistic" ? (
                          <UserBubble text={block.text} source="you" />
                        ) : (
                          <MessageBubble
                            message={block.message}
                            onRetry={handleRetry}
                            onSelectOption={handleSendMessage}
                            highlight={searchQuery}
                            // The in-flight row's card is drawn after the live text instead (see
                            // liftedCard): inside this row it would be stranded mid-reply.
                            cardLifted={
                              !!liftedCard &&
                              block.kind === "message" &&
                              block.message.id === pendingReplyId
                            }
                            // AND ITS PROSE IS LIFTED TOO, but only while the live stream is drawing
                            // the same text — otherwise the reply appears twice (see
                            // mirrorRowProseLifted). The row stays for its tool calls and card.
                            proseLifted={
                              block.kind === "message" &&
                              mirrorRowProseLifted(block.message, pendingReplyId, isStreaming, liveItemsPresent)
                            }
                          />
                        )}
                      </div>
                    );
                  })}

                {/* Live streaming items (text + reasoning chunks) — hidden while a
                    search filter is active, so the view shows only matches. */}
                {!searchActive &&
                  isStreaming &&
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

                {/* THE OPEN QUESTIONS — last, after the live text and every block, because a question comes
                    after the information it is about. See openAsks: a PENDING ask is the live end of the
                    conversation (the turn is parked on it), so it is drawn where the newest thing goes, while
                    a settled record stays interleaved where it happened. */}
                {!searchActive &&
                  openAsks.map((a) => (
                    <div key={`open-ask:${a.key}`} data-testid="ask-card-open">
                      <ConsentAskCard
                        ask={a.ask}
                        outcome={null}
                        busy={!!askInFlight[a.ask.askId]}
                        onDecide={(choice) =>
                          void handleAskDecision(activeConvId!, a.ask.askId, choice)
                        }
                        onAnswer={(text) =>
                          void handleAskDecision(
                            activeConvId!,
                            a.ask.askId,
                            PermissionChoice.UNSPECIFIED,
                            text,
                          )
                        }
                        onEscape={() =>
                          void handleAskDecision(activeConvId!, a.ask.askId, PermissionChoice.DENY)
                        }
                      />
                    </div>
                  ))}

                {/* THE LIFTED CARD — after the live text, because that is where the operator expects the
                    thing they are being asked to answer. See liftedCard for why it cannot live inside the
                    row that records it while a reply is still streaming. */}
                {liftedCard && (
                  <div data-testid="ask-card-live">
                    <AskCard
                      question={liftedCard.question}
                      options={liftedCard.options}
                      allowOther={liftedCard.allowOther}
                      answered={liftedCard.answered}
                      answer={liftedCard.answer}
                      error={liftedCard.error}
                      onSelect={handleSendMessage}
                    />
                  </div>
                )}

                {/* THE ACTIVITY LINE — for the WHOLE turn, not only before the first token.
                    The operator, on the old rule: "After the initial 'Orchicon is thinking...',
                    streaming started and the 'Orchicon is thinking...' went away and never came
                    back." What they lost was the only signal that the stream is ALIVE, and
                    mid-reply is exactly when it matters: a long tool call, a slow provider and a
                    stalled socket look identical from the outside, and with the line gone nothing
                    on screen changes until the reply finishes.

                    The verb ROTATES, indexed on the SERVER's clock: activityVerb takes the last
                    heartbeat's server_time_unix_ms plus only the delta since we received it, so two
                    clients draw the same word for the same server time and a skewed local clock
                    cannot change it. Before the first heartbeat (serverTimeMs === null) the stamp is
                    0 and the selector returns the list's first word — the line is never empty.

                    THE DECISION LIVES IN @/lib/ask-activity-notice, NOT HERE. This route cannot be
                    rendered by the test setup, so logic left inline is logic nothing can assert —
                    the same reason lib/conversationProjects owns the folder-scope rule. It is
                    rendered BELOW the transcript and ABOVE the reconnecting banner, which is the
                    TUI's own order (transcriptStatusLine: reconnecting › disconnected › activity),
                    and activityLineFor returns null while reconnecting so the banner keeps its slot. */}
                {activityLine !== null && (
                  <ActivityLine
                    text={activityLine}
                    announcement={activityAnnouncement}
                    fallbackModel={isUsingFallbackModel ? effectiveModel : null}
                    containerRef={activityLineRef}
                  />
                )}

                {/* Reconnecting notice — the acked turn's socket dropped but
                    the detached collector keeps running server-side. The Stop
                    button + interject input stay available and the poll
                    resolves completion. Shown only while the server confirms
                    the turn is genuinely progressing; when the turn is
                    stalled/wedged (turnProgressing false) the UI reports an
                    accurate stalled state instead of "still working". */}
                {isStreaming && reconnecting && (
                  <div className="flex flex-col items-center gap-1">
                    <p className="text-xs text-muted-foreground">
                      {turnProgressing
                        ? "Connection interrupted — still working… Output continues below. You can interject or stop this reply."
                        : "Turn stalled — the model stopped responding. You can stop this reply and try again."}
                    </p>
                    {/* Live "last activity Ns ago" ticker from the server's
                        turn_last_activity_at (refreshed by the 3s conversations
                        poll) so a re-attached turn proves it is alive — a long,
                        quiet model run looks hung without this. */}
                    {turnProgressing && turnLastActivityAt && (
                      <p
                        className="text-[11px] text-muted-foreground/70"
                        data-testid="turn-last-activity"
                      >
                        Last activity{" "}
                        {formatElapsed(
                          (liveNow - turnLastActivityAt.toDate().getTime()) /
                            1000,
                        )}{" "}
                        ago
                      </p>
                    )}
                  </div>
                )}

                <div className="h-4" />
              </div>
            </ChatScrollContainer>

            {/* Input — fixed at bottom */}
            <div className="shrink-0 border-t pb-2">
              <ChatInputField
                onSend={handleSendMessage}
                onStop={handleStopStreaming}
                isStreaming={isStreaming}
                textareaRef={composerInputRef}
                placeholder="Ask Orchicon Anything..."
                mode={localMode}
                onModeChange={handleModeChange}
                projectOptions={composerProjectOptions}
                projectId={effectiveProjectId}
                onProjectChange={handleComposerProjectChange}
                fullsend={!!activeConv?.fullsend}
                onFullsendChange={handleFullsendChange}
                convId={activeConvId}
                modelRef={askModel}
                onModelChange={handleAskModelChange}
                restoreDraft={restoreDraft}
                onCompact={handleCompactConversation}
                compacting={compactConv.isPending}
              />
            </div>
          </div>
        )}
      </div>

      {/* Right sidebar — conversations panel (w-72 glass-panel, route-local per ADR-0.1) */}
      {!panelCollapsed ? (
        <aside id="conversation-history-panel" data-testid="conversation-history-panel" className="hidden lg:flex w-72 glass-panel rounded-2xl flex-col overflow-hidden border border-black/10 dark:border-white/10 shadow-2xl relative z-20 shrink-0 h-full max-h-full">
          {/* THE HEADER IS TWO ROWS, and that is a FIX rather than a style choice.
              The panel is w-72 (288px) with 14px of padding, so it has 260px of content — and this row was
              carrying a title, a scope dropdown AND three 44px buttons, which needs roughly 410. `justify-between`
              then pushed the last control past the panel's `overflow-hidden` edge. The operator: "the new folder
              icon is cut off."
              Two things make the room. The buttons are 32px rather than 44px targets, which is safe because THIS
              ASIDE IS `hidden lg:flex` — it only exists at desktop widths, and the mobile sheet has its own
              header that keeps its 44px targets for touch. And the scope dropdown gets a row of its own, which is
              what it deserves: it is the workspace picker, and a project name is worth far more legible than
              truncated into whatever sliver the title left. */}
          <div className="p-3.5 border-b border-black/10 dark:border-white/10 shrink-0 flex flex-col gap-2">
            <div className="flex items-center gap-2">
              <div className="flex min-w-0 flex-1 items-center space-x-2 text-muted-foreground">
                <MessageSquare aria-hidden="true" className="w-4 h-4 shrink-0 text-cyan-700 dark:text-cyan-400" />
                <span className="truncate text-xs font-semibold uppercase tracking-wider">Conversations</span>
              </div>
              <div className="flex shrink-0 items-center gap-0.5">
                <button
                  onClick={() => setFolderDialogOpen(true)}
                  className="flex h-8 w-8 items-center justify-center text-muted-foreground hover:text-foreground hover:bg-accent rounded-md transition"
                  title="New folder"
                  aria-label="New folder"
                >
                  <FolderPlus aria-hidden="true" className="w-4 h-4" />
                </button>
                <Link
                  to="/ask-orchicon"
                  search={{ conversationId: undefined } as never}
                  onClick={(e: React.MouseEvent) => { e.preventDefault(); handleNewChat(); }}
                  className="flex h-8 w-8 items-center justify-center text-muted-foreground hover:text-foreground hover:bg-accent rounded-md transition"
                  title="New Chat"
                  aria-label="New conversation"
                >
                  <Plus aria-hidden="true" className="w-4 h-4" />
                </Link>
                <button
                  onClick={togglePanel}
                  aria-expanded={!panelCollapsed}
                  aria-controls="conversation-history-panel"
                  aria-label={panelCollapsed ? "Expand conversation history" : "Collapse conversation history"}
                  className="flex h-8 w-8 items-center justify-center text-muted-foreground hover:text-foreground hover:bg-accent rounded-md transition focus:outline-none focus-visible:ring-2 focus-visible:ring-cyan-400/30"
                  title="Collapse Panel"
                >
                  <PanelRightClose aria-hidden="true" className="w-4 h-4" />
                </button>
              </div>
            </div>
            {/* THE PROJECT SCOPE DROPDOWN. The operator: "a dropdown at the top of the conversation bar that
                allows you to pick a project, and then under that project you would only see THAT PROJECT'S
                Conversations and Categories. Projects are WORKSPACES essentially." Full width, on its own row,
                so the project name is readable. */}
            <ProjectScopeSelect
              fullWidth
              options={projectScopeOptions}
              value={projectScope}
              onChange={setProjectScope}
            />
          </div>
          <div className="flex-1 overflow-y-auto p-2 space-y-1">
          {convsLoading && (
            <p className="text-xs text-center text-muted-foreground py-4">
              Loading...
            </p>
          )}
          {!convsLoading && scopedConversations.length === 0 && (
            <p className="text-xs text-center text-muted-foreground py-4">
              {projectScope === ALL_PROJECTS
                ? "No conversations yet"
                : `No conversations in ${scopeLabel(projectScope, projectScopeOptions)}`}
            </p>
          )}

          <DndContext
            sensors={dndSensors}
            collisionDetection={pointerWithin}
            onDragStart={handleDragStart}
            onDragOver={handleDragOver}
            onDragEnd={handleDragEnd}
            onDragCancel={handleDragCancel}
          >
            <SortableContext
              items={scopedConversations.map((c) => c.id)}
              strategy={verticalListSortingStrategy}
            >
              {/* THE CATEGORY FOLDERS, WITHIN THE SELECTED PROJECT. The operator: "a conversation would belong
                  to a project and inside the project it would still have the normal categories we had before."
                  These folders are the SAME machinery as before — the only thing that changed is that they now
                  receive the SCOPED set of conversations, so each holds only this project's chats.
                  AND THE LIST IS SCOPE-FILTERED, not the full category set: a folder holding nothing in this
                  workspace is not part of it. See visibleCategories. */}
              {visibleCategories.map((category) => {
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
                    renderMoveControl={renderMoveControl}
                    runningIds={runningIds}
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
                hasFolders={visibleCategories.length > 0}
                renderMoveControl={renderMoveControl}
                runningIds={runningIds}
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
          <div className="p-2 border-t border-black/10 dark:border-white/10 bg-black/[0.03] dark:bg-slate-900/30 shrink-0">
            <button
              onClick={handleNewChat}
              className="w-full flex items-center justify-center space-x-2 py-1.5 px-3 rounded-xl bg-accent/50 hover:bg-accent text-muted-foreground hover:text-foreground text-xs font-medium transition border border-border/50"
            >
              <History aria-hidden="true" className="w-3.5 h-3.5 text-muted-foreground" />
              <span>View All History</span>
            </button>
          </div>
        </aside>
      ) : (
        <button
          onClick={togglePanel}
          aria-expanded={!panelCollapsed}
          aria-controls="conversation-history-panel"
          aria-label="Expand conversation history"
          title="Expand Conversations"
          className="hidden lg:flex h-fit p-2 glass-panel rounded-xl text-muted-foreground hover:text-foreground hover:bg-accent transition self-start focus:outline-none focus-visible:ring-2 focus-visible:ring-cyan-400/30"
        >
          <PanelRight aria-hidden="true" className="h-4 w-4" />
        </button>
      )}

      {/* Mobile conversation sheet — route-scoped, lg:hidden per ADR-M10-01 */}
      {mobileSheetOpen && (
        <div className="fixed inset-0 z-50 lg:hidden" role="dialog" aria-modal="true" aria-label="Conversations">
          <div className="absolute inset-0 bg-black/50" onClick={closeMobileSheet} aria-hidden="true" />
          <div className="absolute inset-y-0 right-0 w-[85vw] max-w-[360px] h-[100dvh] max-h-[100dvh] pb-[env(safe-area-inset-bottom,0px)] glass-panel rounded-l-2xl flex flex-col overflow-hidden border border-black/10 dark:border-white/10 shadow-2xl animate-in slide-in-from-right duration-200">
            <div className="p-3.5 border-b border-black/10 dark:border-white/10 flex items-center justify-between shrink-0">
              <div className="flex items-center space-x-2 text-muted-foreground">
                <MessageSquare aria-hidden="true" className="w-4 h-4 text-cyan-700 dark:text-cyan-400" />
                <span className="text-xs font-semibold uppercase tracking-wider">Conversations</span>
              </div>
              <div className="flex items-center space-x-1">
                {/* The mobile sheet is the SAME surface as the desktop panel, so it gets the same scope
                    control — otherwise a phone user would see every project's conversations at once and no way
                    to narrow them. "All projects" is dropped here: the sheet is narrow, and its purpose is to
                    put you in ONE project, so the unassigned scope stands in for it. */}
                <ProjectScopeSelect
                  options={projectScopeOptions.filter((o) => o.value !== ALL_PROJECTS)}
                  value={projectScope === ALL_PROJECTS ? "" : projectScope}
                  onChange={setProjectScope}
                  label="Project workspace"
                />
                {/* Mirrored here so a phone user can reach the open
                    conversation's MCP servers + skill files too — the desktop
                    header control is off-screen in the sheet layout. */}
                <ConversationScopeDisclosure conversationId={activeConvId ?? ""} />
                <button onClick={() => setFolderDialogOpen(true)} className="flex h-11 w-11 min-h-[44px] min-w-[44px] items-center justify-center text-muted-foreground hover:text-foreground hover:bg-accent rounded-md transition" title="New folder" aria-label="New folder"><FolderPlus aria-hidden="true" className="w-4 h-4" /></button>
                <button onClick={() => { setMobileSheetOpen(false); handleNewChat(); }} className="flex h-11 w-11 min-h-[44px] min-w-[44px] items-center justify-center text-muted-foreground hover:text-foreground hover:bg-accent rounded-md transition" title="New Chat" aria-label="New conversation"><Plus aria-hidden="true" className="w-4 h-4" /></button>
                <button onClick={closeMobileSheet} className="flex h-11 w-11 min-h-[44px] min-w-[44px] items-center justify-center text-muted-foreground hover:text-foreground hover:bg-accent rounded-md transition" title="Close" aria-label="Close conversations"><PanelRightClose aria-hidden="true" className="w-4 h-4" /></button>
              </div>
            </div>
            <div className="flex-1 overflow-y-auto p-2 space-y-1">
              {convsLoading && <p className="text-xs text-center text-muted-foreground py-4">Loading...</p>}
              {!convsLoading && scopedConversations.length === 0 && <p className="text-xs text-center text-muted-foreground py-4">{projectScope === ALL_PROJECTS ? "No conversations yet" : `No conversations in ${scopeLabel(projectScope, projectScopeOptions)}`}</p>}
              <DndContext sensors={dndSensors} collisionDetection={pointerWithin} onDragStart={handleDragStart} onDragOver={handleDragOver} onDragEnd={handleDragEnd} onDragCancel={handleDragCancel}>
                <SortableContext items={scopedConversations.map((c) => c.id)} strategy={verticalListSortingStrategy}>
                  {visibleCategories.map((category) => {
                    const folderConvIds = categorizedConversations.categorized.get(category.id) ?? [];
                    const isCollapsed = convPrefs.collapsed.has(category.id);
                    const isOver = overFolderId === category.id;
                    const isRenaming = renamingFolderId === category.id;
                    return (
                      <FolderItem key={category.id} id={category.id} name={category.name} isCollapsed={isCollapsed} isOver={isOver} isRenaming={isRenaming} renameValue={folderRenameValue} renameInputRef={folderRenameInputRef} onToggle={() => convPrefs.toggleCollapsed(category.id)} onStartRename={() => startRenameFolder(category.id, category.name)} onSaveRename={() => saveRenameFolder(category.id)} onCancelRename={cancelRenameFolder} onRenameChange={setFolderRenameValue} onDelete={() => convPrefs.deleteCategory(category.id)} convIds={folderConvIds} convById={convById} activeConvId={activeConvId} renamingConvId={renamingConvId} convRenameValue={renameValue} convRenameInputRef={renameInputRef} onSelectConv={(id) => { setMobileSheetOpen(false); setActiveConvId(id); }} onStartRenameConv={startRenameConv} onSaveRenameConv={saveRenameConv} onCancelRenameConv={cancelRenameConv} onRenameConvChange={setRenameValue} onDeleteConv={handleDeleteConv} onStopConv={handleStopConversation} activeDragId={activeDragId} renderMoveControl={renderMoveControl} runningIds={runningIds} />
                    );
                  })}
                  <UncategorizedDropZone id="__uncategorized__" convIds={categorizedConversations.uncategorized} convById={convById} activeConvId={activeConvId} renamingConvId={renamingConvId} renameValue={renameValue} renameInputRef={renameInputRef} onSelectConv={(id) => { setMobileSheetOpen(false); setActiveConvId(id); }} onStartRenameConv={startRenameConv} onSaveRenameConv={saveRenameConv} onCancelRenameConv={cancelRenameConv} onRenameConvChange={setRenameValue} onDeleteConv={handleDeleteConv} onStopConv={handleStopConversation} activeDragId={activeDragId} isOver={overFolderId === "__uncategorized__"} hasFolders={visibleCategories.length > 0} renderMoveControl={renderMoveControl} runningIds={runningIds} />
                </SortableContext>
                <DragOverlay dropAnimation={null}>
                  {activeDragId ? <div className="rounded-md bg-background border shadow-md px-3 py-2 text-sm text-foreground max-w-[200px] truncate">{convById.get(activeDragId)?.title || "New conversation"}</div> : null}
                </DragOverlay>
              </DndContext>
            </div>
            <div className="p-2 border-t border-black/10 dark:border-white/10 bg-black/[0.03] dark:bg-slate-900/30 shrink-0">
              <button onClick={() => { setMobileSheetOpen(false); handleNewChat(); }} className="w-full flex items-center justify-center space-x-2 py-3 px-3 rounded-xl bg-accent/50 hover:bg-accent text-muted-foreground hover:text-foreground text-xs font-medium transition border border-border/50 min-h-[44px]">
                <History aria-hidden="true" className="w-3.5 h-3.5 text-muted-foreground" />
                <span>View All History</span>
              </button>
            </div>
          </div>
        </div>
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

/**
 * askCardOf returns a row's clarifying card — the recorded ask_user call — when it should be drawn.
 *
 * IT EXISTS SO THE CARD CAN BE RENDERED SOMEWHERE OTHER THAN INSIDE THE ROW, and that separation is the
 * fix for "it's at the top of the text that was sent rather than at the bottom after the text".
 *
 * The card is the last thing inside MessageBubble, which is correct WHEN THE ROW IS THE WHOLE TURN. It is
 * wrong while a reply is still streaming: the row the card lives on sorts BEFORE the live text (it predates
 * it), so drawing the card inside that row puts it between the row's own prose and the live copy — the card
 * ends up stranded mid-reply, above text that arrived after the question was asked.
 *
 * So the caller lifts the card out of the row and emits it after the live text, which is where the operator
 * expects it (the newest thing, the thing being asked of them). The row keeps its prose and its reasoning.
 */
function askCardOf(message: ChatMessage) {
  const call = (message.toolCalls ?? []).find((c) => isAskUserToolCall(c.functionName));
  if (!call) return null;
  const parsed = parseAskUserArgs(call.arguments);
  const result = (message.toolResults ?? []).find((r) => r.toolCallId === call.id);
  const plan = askCardPlan(true, !!parsed, !!result);
  if (!plan.render) return null;
  return {
    id: call.id,
    question: parsed?.question ?? "",
    options: parsed?.options ?? [],
    allowOther: parsed?.allowOther,
    answered: !!result,
    answer: result?.output ?? "",
    error: plan.error,
  };
}

function MessageBubble({
  message,
  onRetry,
  onSelectOption,
  highlight = "",
  cardLifted = false,
  proseLifted = false,
}: {
  message: ChatMessage;
  onRetry?: () => void;
  // onSelectOption sends a clicked clarifying-question option as the answer to the
  // PAUSED turn (it goes over the reply RPC as the ask_user tool result).
  onSelectOption?: (label: string) => void;
  // highlight is the active in-transcript search query. It is threaded to the bubbles so a match is
  // marked WHERE IT IS: the filter already answers "which message", and on a long reply the operator
  // needs "which line". Empty (no search) renders exactly as before.
  highlight?: string;
  /**
   * cardLifted tells this bubble its clarifying card is being drawn ELSEWHERE (after the live text) and it
   * must not draw its own copy. See askCardOf: while a reply streams, the row the card lives on sorts before
   * the live text, so drawing the card inside the row strands it mid-reply.
   */
  cardLifted?: boolean;
  /**
   * proseLifted tells this bubble its TEXT and REASONING are being drawn by the live stream and it
   * must not draw its own copy.
   *
   * The in-flight row is kept in the transcript even while the reply streams, because it carries the
   * turn's tool calls and its recorded ask_user card — and the live stream carries neither. But it also
   * carried a 250ms-old snapshot of the very text the live stream is drawing, so the reply was painted
   * twice: once as the row's prose, once as the growing live copy. The operator: "while the model is
   * responding/thinking, it prints the message twice." See mirrorRowProseLifted.
   */
  proseLifted?: boolean;
}) {
  // WHICH SHELL DRAWS THIS MESSAGE. Decided in one tested place (lib/ask-bubble.ts)
  // rather than by a fall-through: `system` is the PLATFORM speaking about the
  // conversation — today a compaction record — and it used to land in the assistant's
  // bubble, claiming for the model the one event where attribution is the point.
  const kind = bubbleKindFor(message);

  if (kind === "user") {
    return <UserBubble text={message.content} source="you" highlight={highlight} />;
  }

  if (kind === "notice") {
    return <NoticeBubble text={message.content} />;
  }

  if (kind === "error") {
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
              <RefreshCw aria-hidden="true" className="h-3.5 w-3.5 mr-1" />
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

  // A recorded ask_user call renders as the clarifying-question card. It is
  // interactive only while NOT answered; selecting an option sends its label as the
  // answer to the PAUSED turn.
  const askCall = (message.toolCalls ?? []).find((c) =>
    isAskUserToolCall(c.functionName),
  );
  const askParsed = askCall ? parseAskUserArgs(askCall.arguments) : null;
  // ANSWERED IS READ FROM THE ASK'S OWN RESULT, not from a later message.
  //
  // It used to be `answered={message.id !== lastMessageId}` — "a later message
  // exists" — which was right while answering SENT the choice as the next user
  // message. With ask_user made BLOCKING the answer arrives as the TOOL RESULT and
  // nothing follows the message, so that test stayed false forever and the card never
  // settled. The operator: "the Orchicon asks card does not go away in the TUI or the
  // GUI when you select something."
  //
  // The result is the ask's own outcome — server truth — rather than an inference from
  // what happened afterwards.
  const askResult = askCall
    ? (message.toolResults ?? []).find((r) => r.toolCallId === askCall.id)
    : undefined;
  const askAnswered = !!askResult;
  // WHETHER TO DRAW AT ALL, and whether to draw as an error. The distinction is the reported bug: a
  // call recorded but NOT YET RESOLVED carries a placeholder `{}` for its arguments, and for
  // `ask_user` that state lasts as long as the question is open (the call BLOCKS) — so the
  // transcript held an unparseable card for the whole time the operator was looking at the real
  // one. See askCardPlan.
  const askPlan = askCardPlan(!!askCall, !!askParsed, !!askResult);
  // The answer ITSELF, so the settled record can state it. It is the ask's own tool result —
  // server truth, present in the persisted transcript — rather than a guess from a later
  // message, which is the same rule that fixed "answered" in the first place.
  const askAnswer = askResult?.output ?? "";

  return (
    <>
      {hasReasoning && !proseLifted && (
        <ReasoningBubble text={reasoning!.join("\n")} />
      )}
      {/* A running turn's mirrored partial row can hold reasoning but no text
          yet (the model is still thinking) — skip the empty bubble shell. */}
      {message.content && !proseLifted && (
        <AssistantBubble
          text={message.content}
          label="Orchicon"
          highlight={highlight}
        />
      )}
      {/* THE QUESTION CARD IS THE LAST THING IN THE MESSAGE.

          It is the thing the operator ACTS ON, not a transcript note buried
          above the prose that followed the call. The tool call is chronologically
          first, but reading order is not chronology — the operator: "it is ON TOP
          of a bunch of other text you sent. That is not intuitive. It should be at
          the bottom (newest/recent)." The TUI emits its card last for the same
          reason (chat.conversationItems), so the two clients read the same way. */}
      {askCall && askPlan.render && !cardLifted && (
        <AskCard
          question={askParsed?.question ?? ""}
          options={askParsed?.options ?? []}
          allowOther={askParsed?.allowOther}
          answered={askAnswered}
          answer={askAnswer}
          error={askPlan.error}
          onSelect={onSelectOption}
        />
      )}
    </>
  );
}

// parseComposerCommand lives in @/lib/composer-command (a pure, unit-tested
// module) because the grammar must stay identical to the TUI's ParseSlash — see
// internal/tui/slash.go. It is imported at the top of this file.

// --- ChatInputField: auto-resizing textarea with attach/voice/send ---

function ChatInputField({
  onSend,
  onStop,
  isStreaming,
  placeholder = "Ask Orchicon anything...",
  mode = ConversationMode.BRAINSTORM,
  onModeChange,
  projectOptions,
  projectId = "",
  onProjectChange,
  fullsend = false,
  onFullsendChange,
  convId,
  modelRef = "",
  onModelChange,
  restoreDraft,
  onCompact,
  compacting = false,
  textareaRef,
}: {
  onSend: (text: string, attachments?: AttachmentInput[]) => Promise<boolean>;
  onStop: () => void;
  isStreaming: boolean;
  placeholder?: string;
  mode?: ConversationMode;
  onModeChange?: (mode: ConversationMode) => void;
  // THE PROJECT CONTROL on the composer row, alongside the mode. It is supplied whenever there is a
  // list of projects to choose from, which includes the HERO (no conversation yet) — that is the
  // whole point, because choosing the workspace at creation is what was missing. OnChange holds the
  // choice before a conversation exists and applies it immediately once one does.
  projectOptions?: ScopeOption[];
  projectId?: string;
  onProjectChange?: (projectId: string) => void;
  // fullsend is the conversation's FULLSEND state AS THE SERVER REPORTS IT, and
  // onFullsendChange writes it. Both are SUPPLIED ONLY WHEN A CONVERSATION IS OPEN: there is no
  // pending form (see handleFullsendChange), so the control is absent on the hero rather than
  // present and inert.
  fullsend?: boolean;
  onFullsendChange?: (on: boolean) => void;
  convId?: string | null;
  // modelRef is the model answering this conversation. The session stat strip
  // reports it alongside the context / tokens / cache / cost numbers.
  modelRef?: string;
  // onModelChange retargets the model. The PARENT decides the meaning: with a
  // conversation open it writes that conversation, and on the hero (no
  // conversation yet) it holds the choice for the create.
  onModelChange?: (ref: string) => void;
  // When the parent detects a reply failure (a turn that was acked but whose
  // reply errored), it signals this with the sent text so the composer puts
  // it back in the box. Null/absent = nothing to restore.
  restoreDraft?: { convId: string; text: string; token: number } | null;
  // onCompact runs the /compact command (free context by compacting this
  // conversation's history). It resolves to the OUTCOME: `compacted`
  // distinguishes a real compaction from a server-side DECLINE (whose reason
  // arrives in `detail`), so a decline is not dressed up as a success.
  onCompact?: (convId: string) => Promise<{ detail: string; compacted: boolean }>;
  // compacting reports that a compaction is IN FLIGHT, so the control can show
  // progress during the wait instead of sitting mute and greyed. The parent
  // derives it from the mutation's OWN lifecycle (isPending), which settles
  // with the request, rather than a flag this component must remember to clear.
  compacting?: boolean;
  // textareaRef exposes the composer's textarea to the page, so the transcript
  // search's Escape can return focus here. Optional: the hero composer passes none.
  textareaRef?: MutableRefObject<HTMLTextAreaElement | null>;
}) {
  // The input stays ENABLED while streaming: sending mid-reply is the
  // interject path (interrupt + redirect), not a rejected "already
  // processing" send. Only the Stop button is offered alongside Send.
  const [text, setText] = useState("");
  const [attachments, setAttachments] = useState<AttachmentInput[]>([]);
  const [sending, setSending] = useState(false);
  const inputRef = useRef<HTMLTextAreaElement | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // The session stat strip (ask model · context · tokens · cache · cost). It
  // re-reads when a turn COMPLETES — a finished turn is exactly when new usage
  // lands — and when the conversation changes.
  const { line: metricsLine, stats: statsLine } = useAskMetricsLive(convId, modelRef, isStreaming);

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

  // When the running compaction started (ms since epoch). The RPC is UNARY and
  // server-side it runs a summarize MODEL CALL, so it can take tens of seconds
  // and reports no intermediate progress at all. The control therefore shows an
  // ELAPSED timer, not a percentage: elapsed time is the one progress fact the
  // client actually has, and a fabricated progress bar would imply a
  // measurement the server never sends.
  const [compactStartedAt, setCompactStartedAt] = useState<number | null>(null);

  // runCompact frees this conversation's context (the /compact action). ONE
  // implementation, shared by the typed command and the toolbar button, so the
  // two can never drift on the guards — the same reason the TUI keeps a single
  // command path.
  //
  // It deliberately does NOT touch the composer text: the typed path clears the
  // box itself (the command text was consumed), while the button must never wipe
  // a draft the operator is in the middle of writing.
  const runCompact = useCallback(async () => {
    if (!convId) {
      useToastStore.getState().push({ kind: "error", message: "No conversation open — send a message first." });
      return;
    }
    // Refuse mid-turn: compaction rewrites the history the running turn is
    // generating from, so doing it underneath a live answer would corrupt it.
    // (The TUI refuses identically — CanCompact.)
    if (isStreaming) {
      useToastStore.getState().push({
        kind: "error",
        message: "A turn is in flight — stop it before /compact (compaction rewrites the history the turn is using).",
      });
      return;
    }
    if (!onCompact) return;
    setSending(true);
    setCompactStartedAt(Date.now());
    try {
      const res = await onCompact(convId);
      // A DECLINE is not an achievement: the server reports compacted=false with
      // its reason ("only N messages so far"), and rendering that as a green
      // success toast reads as if something happened when nothing did.
      useToastStore.getState().push({
        kind: res.compacted ? "success" : "info",
        message: res.detail || (res.compacted ? "conversation compacted" : "nothing to compact"),
      });
    } catch (err) {
      useToastStore.getState().push({
        kind: "error",
        message: `/compact failed: ${err instanceof Error ? err.message : String(err)}`,
      });
    } finally {
      setSending(false);
      setCompactStartedAt(null);
    }
  }, [convId, isStreaming, onCompact]);

  // Why compaction is unavailable right now ("" = available). Surfacing the
  // reason on the control BEFORE the click beats a toast AFTER it — these are
  // the same two conditions runCompact refuses on.
  // A RUNNING compaction is deliberately NOT one of the refusal reasons. It used
  // to report "Working…" here, which rendered as a dead greyed button: nothing
  // moved, so the operator could not tell work from a hang until the completion
  // toast appeared. While compacting, the control reports its own progress (see
  // the button) instead of a refusal — the disabled state stays (a second fire
  // would race the rewrite), but it is no longer mute.
  const compactBlocked = !convId
    ? "Open a conversation first"
    : isStreaming
      ? "A turn is in flight — stop it before compacting"
      : !compacting && sending
        ? "Working…"
        : "";

  // Disabled while compacting as well as when refused, so the in-flight state
  // cannot double-fire — but it is the PROGRESS rendering that fills the wait.
  const compactDisabled = compacting || compactBlocked !== "";

  const handleSubmit = useCallback(async () => {
    // The sending lock only guards a double-click on a FRESH send. While a
    // turn is streaming, `sending` stays true for the whole turn (onSend
    // resolves only when the stream ends), so locking unconditionally would
    // swallow every interject attempt mid-reply — the box already cleared on
    // submit, so a submit during a stream IS a deliberate interject
    // (interrupt + redirect), not an accidental duplicate.
    if (sending && !isStreaming) return;
    if (pendingReads > 0) {
      useToastStore.getState().push({ kind: "info", message: `Still loading ${pendingReads} file(s)...` });
      return;
    }
    const sentText = text.trim();
    const sentAttachments = attachments.length > 0 ? attachments : undefined;

    // Composer commands are intercepted BEFORE the send path, so a command is
    // never delivered to the model as a chat message. Only /compact exists
    // today; an unknown /word intentionally falls through to chat (the server
    // is the authority on what a message is, and silently swallowing an
    // unrecognized slash would lose the user's text).
    const cmd = parseComposerCommand(sentText);
    if (cmd && cmd.name === COMPACT_COMMAND) {
      setText("");
      if (inputRef.current) inputRef.current.style.height = "auto";
      await runCompact();
      return;
    }

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
  }, [sending, text, attachments, onSend, pendingReads, isStreaming, convId, onCompact, runCompact]);

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
            } catch { /* inline base64 decode is best-effort — fall through to no-preview */ }
            return (
              <span
                key={i}
                className="inline-flex items-center gap-1 rounded-md bg-muted px-2 py-1 text-xs"
              >
                {isImage && previewUrl ? (
                  <img src={previewUrl} alt={a.name} className="h-8 w-8 rounded object-cover" />
                ) : (
                  <Paperclip aria-hidden="true" className="h-3 w-3" />
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
      {/* NO `overflow-hidden` ON THIS BOX, and the removal is the fix rather than a tidy-up.
          It clipped the composer row's popovers: the project pill's menu opens BELOW the row, and with the
          box clipping at its own rounded edge the menu was cut off mid-item — "the project dropdown is being
          swallowed by the bubble. It needs to be on top." The same box wraps the MODE control, which grows the
          same kind of popover, so both were being clipped by one rule.
          The radii that `overflow-hidden` was really there for (the glass panel's inner content meeting its
          rounded border) are preserved by the `rounded-2xl` on the glass surface itself; nothing inside this
          box paints past the corners, so removing the clip costs no visual containment and restores the
          popovers. */}
      <div
        className={`glass-input rounded-2xl p-2.5 ${dragOver ? "ring-2 ring-cyan-400 border-dashed" : ""}`}
        onDragOver={handleDragOver}
        onDragLeave={handleDragLeave}
        onDrop={handleDrop}
      >
        {/* Textarea area */}
        <div className="flex items-end gap-2 px-1 pb-1">
          <Sparkles aria-hidden="true" className="h-4 w-4 text-cyan-700 dark:text-cyan-400 shrink-0 mb-1.5" />
          <button
            type="button"
            onClick={() => fileInputRef.current?.click()}
            className="shrink-0 rounded-md border border-black/10 dark:border-white/10 bg-black/[0.04] dark:bg-white/5 p-1.5 text-muted-foreground hover:text-cyan-700 dark:hover:text-cyan-300 hover:bg-black/5 dark:hover:bg-white/10 transition-colors"
            title="Attach file"
            aria-label="Attach file"
          >
            <Paperclip aria-hidden="true" className="h-4 w-4" />
          </button>
          <textarea
            ref={(el) => {
              inputRef.current = el;
              if (textareaRef) textareaRef.current = el;
            }}
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
            <Mic aria-hidden="true" className="h-4 w-4" />
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
                  <Square aria-hidden="true" className="h-3.5 w-3.5 mr-1" />
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
            {/* Compact belongs with the ACTIONS (left), not the conversation
                state (right): the right group already carries the model, the
                session stats and the mode. Disabled — with the reason on the
                control — when there is nothing to compact or a turn is live,
                which is exactly when the typed command would refuse.
                While it RUNS the control must read as alive, not dead: the
                shared disabled style dims to 50% opacity, which is exactly what
                made a long compaction look like a broken button, so the busy
                state restores full opacity and tints the label. cn is
                tailwind-merge, so disabled:opacity-100 supersedes the base
                disabled:opacity-50 rather than stacking with it. */}
            {onCompact && (
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => void runCompact()}
                disabled={compactDisabled}
                title={
                  compacting
                    ? "Compacting… the server is summarizing this conversation (it runs a model call, so this can take a while)"
                    : compactBlocked ||
                      "Compact this conversation — frees context; the server decides if it can"
                }
                aria-label="Compact conversation"
                aria-busy={compacting}
                data-testid="ask-compact"
                data-compacting={compacting ? "true" : "false"}
                className={
                  compacting
                    ? "disabled:opacity-100 text-cyan-600 dark:text-cyan-400"
                    : undefined
                }
              >
                {compacting ? (
                  <>
                    <Loader2
                      aria-hidden="true"
                      className="h-3.5 w-3.5 mr-1 animate-spin"
                    />
                    Compacting
                    {compactStartedAt !== null && (
                      <LiveDuration
                        startedAt={compactStartedAt}
                        className="ml-1 font-mono text-xs text-muted-foreground"
                      />
                    )}
                  </>
                ) : (
                  <>
                    <Minimize2 aria-hidden="true" className="h-3.5 w-3.5 mr-1" />
                    Compact
                  </>
                )}
              </Button>
            )}
          </div>
          <div className="flex items-center gap-2 min-w-0">
            {/* The session stat strip: the ask model as a CLICKABLE chip (it
                opens the model picker), then the numeric stats. Right-aligned
                and immediately BEFORE the mode dropdown (the operator's
                placement ask). The numbers hide first on a narrow box — the chip
                is the CONTROL and must survive. */}
            {/* ALWAYS RENDERED, INCLUDING WHEN NO MODEL IS CONFIGURED — because the chip IS the
                picker. Gating it on `modelRef` was a regression I introduced: with no default Ask
                model set, `modelRef` is empty, so the control disappeared and there was no way to
                choose a model from the conversation at all. The operator: "If no default ask model
                is set, it doesn't show a picker in the conversation. That is the wrong approach. We
                should definitely let people still choose from the conversation if no default is
                set."
                The comment below already said the chip "is the CONTROL and must survive"; the gate
                contradicted it. An empty ref is the state that needs the control MOST, not least. */}
            <AskModelChip model={modelRef} onModelChange={onModelChange} />
            {statsLine && (
              <span
                className="hidden truncate font-mono text-[11px] text-muted-foreground sm:inline"
                title={metricsLine}
              >
                {statsLine}
              </span>
            )}
            {onFullsendChange && (
              // FULLSEND IS TOGGLEABLE MID-TURN, deliberately, and the `disabled={isStreaming}`
              // that used to be here was a mistake I introduced by reflex: I copied the
              // constraint from the MODEL picker beside it, which really cannot change mid-turn
              // (the running session belongs to the model that opened it). Fullsend has no such
              // constraint — it is a plain flag the consent layer reads at EACH DECISION
              // (internal/askorchicon/consent.go) and the bash guard re-reads per invocation, so a
              // change lands on the very next ask with no session or turn restart.
              //
              // AND MID-TURN IS ITS PRIMARY USE CASE. Nobody thinks "I should turn fullsend on
              // before I start" — you reach for it when you are already mid-task and being asked
              // too often, which is precisely when the operator found it greyed out. The mode
              // dropdown on the same row was never disabled, so the two siblings disagreed about
              // what a running turn allows.
              <FullsendToggle
                on={!!fullsend}
                onChange={onFullsendChange}
              />
            )}
            {/* THE PROJECT PILL SITS BESIDE THE MODE, because they answer the same kind of question
                at the same moment: which persona, and which workspace. It reuses the sidebar's
                ProjectScopeSelect so the option list, the archived marker and the unknown-project
                fallback are decided in ONE place — and it opens LEFTWARD-safe from this row (the
                trigger is the rightmost control in the composer). */}
            {onProjectChange && projectOptions && projectOptions.length > 0 && (
              <ProjectScopeSelect
                options={projectOptions}
                value={projectId}
                onChange={onProjectChange}
                label="Conversation project"
              />
            )}
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
  /** The conversation's project, for the row's move control. */
  projectId?: string;
  /**
   * Builds the row's "move to project" control. A render prop rather than a callback, because the control is a
   * popover with its own open state — constructing it per row is what lets it reuse the SAME component the
   * sidebar header uses, so one place decides the option list, the archived marker and the unknown-project
   * fallback.
   */
  renderMoveControl?: (convId: string, projectId: string) => React.ReactNode;
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
  projectId,
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
  renderMoveControl,
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
            ? "bg-cyan-500/10 border-cyan-500/30 text-foreground"
            : "border-transparent text-muted-foreground hover:bg-accent hover:border-border hover:text-foreground",
        )}
      >
        <div className="flex items-start gap-2">
          {/* Drag handle */}
          <span
            {...attributes}
            {...listeners}
            className="shrink-0 mt-0.5 cursor-grab active:cursor-grabbing opacity-0 group-hover:opacity-100 text-muted-foreground"
          >
            <GripVertical aria-hidden="true" className="h-3 w-3" />
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
                    <span aria-hidden="true" className="animate-ping absolute inline-flex h-full w-full rounded-full bg-sky-400 opacity-75" />
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
                    <Square aria-hidden="true" className="h-3 w-3 fill-current" />
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
            <span className="shrink-0 flex items-center gap-0.5">
              {/* MOVE TO PROJECT — ALWAYS VISIBLE, where rename and delete stay hover-revealed.
                  The sidebar is a SCOPED view rather than one folder per project, so there is no project folder
                  to drag a conversation onto; this control is the only way a chat changes project. The operator,
                  on the version where it was hover-gated: "I also don't think there is currently a gui way to
                  move a conversation to a different project like in the tui." They were right — it existed but
                  only appeared under the pointer, so the affordance was indistinguishable from absent. An action
                  with no other route cannot be hidden behind a hover. */}
              {renderMoveControl?.(convId, projectId ?? "")}
              <span className="flex items-center gap-0.5 opacity-0 group-hover:opacity-100 focus-within:opacity-100">
                <button
                  onClick={onStartRename}
                  className="text-muted-foreground hover:text-foreground"
                  title="Rename"
                >
                  <Pencil aria-hidden="true" className="h-3 w-3" />
                </button>
                <button
                  onClick={onDelete}
                  className="text-muted-foreground hover:text-destructive"
                  title="Delete"
                >
                  <Trash2 aria-hidden="true" className="h-3 w-3" />
                </button>
              </span>
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
  /** Builds each member row's "move to project" control — see ConversationItemProps.renderMoveControl. */
  renderMoveControl?: (convId: string, projectId: string) => React.ReactNode;
  /**
   * The conversation ids the rail should show as running — the UNION of the server's polled turnInFlight and
   * this client's own live stream slots (lib/ask-running.ts). Passed down rather than re-derived per row so
   * every list (folded, uncategorized, mobile) reads ONE decision.
   */
  runningIds: Set<string>;
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
  renderMoveControl,
  runningIds,
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
            aria-hidden="true" className={cn(
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
              <Pencil aria-hidden="true" className="h-3 w-3" />
            </button>
            <button
              onClick={onDelete}
              className="text-muted-foreground hover:text-destructive"
              title="Delete folder"
            >
              <Trash2 aria-hidden="true" className="h-3 w-3" />
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
                projectId={conv.projectId ?? ""}
                renderMoveControl={renderMoveControl}
                lastMessagePreview={conv.lastMessagePreview}
                isRunning={runningIds.has(convId)}
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
  /** Builds each row's "move to project" control — see ConversationItemProps.renderMoveControl. */
  renderMoveControl?: (convId: string, projectId: string) => React.ReactNode;
  /** See FolderItemProps.runningIds — the union the rail rows read. */
  runningIds: Set<string>;
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
  renderMoveControl,
  runningIds,
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
            projectId={conv.projectId ?? ""}
            renderMoveControl={renderMoveControl}
            lastMessagePreview={conv.lastMessagePreview}
            isRunning={runningIds.has(convId)}
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
