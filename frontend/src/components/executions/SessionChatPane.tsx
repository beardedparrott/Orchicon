// SessionChatPane — the live, durable worker session view (Stage 3).
//
// Ask-Orchicon-grade chat for an execution's opencode session:
//   - user messages (goal, liveness nudges, mid-run human messages) are
//     right-aligned primary bubbles;
//   - assistant text is left-aligned card bubbles with markdown + copy;
//   - tool calls are compact collapsible cards, reasoning is collapsed,
//     artifacts render inline;
//   - one full-height column that auto-sticks to the bottom while
//     streaming (with a jump-to-bottom affordance when scrolled up);
//   - a composer on the running pane that injects a mid-run message into
//     the live session (SendExecutionMessage — no new execution/work
//     item), plus Stop.
//
// Sources: while running, the live event stream renders instantly and the
// durable session transcript (execution_session_parts) supplies the user
// messages. Once the execution is terminal, the pane renders the
// transcript alone — the full conversation survives the serve/container
// lifecycle, so a finished execution looks identical to the live view and
// can be reviewed later for curiosity or troubleshooting.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ArrowDown,
  Copy,
  SendHorizontal,
  Square,
  TerminalSquare,
} from "lucide-react";
import type { StreamExecutionEventsResponse } from "@/api/gen/orchicon/api/v1/execution_pb";
import { useGetExecutionSession, useSendExecutionMessage, useContinueExecutionSession } from "@/api/executions";
import { ReasoningBubble } from "@/components/chat";
import { Markdown } from "@/components/markdown";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

import { groupByPhase, mergeSessionItems, type ChatItem, type ParsedTool } from "./sessionItems";

interface SessionChatPaneProps {
  executionId: string;
  events: StreamExecutionEventsResponse[];
  streamStatus?: string;
  storedOutput?: string;
  workerName?: string;
  /** The full composite system prompt sent to the worker (system field).
   *  Rendered as the first, collapsible bubble so the operator can verify
   *  exactly what the worker was told. */
  systemPrompt?: string;
  isRunning: boolean;
  isTerminal: boolean;
}

// --- transcript → chat items --------------------------------------------

function decodePayload(payload?: Uint8Array) {
  if (!payload?.length) return {};
  try {
    return JSON.parse(new TextDecoder().decode(payload));
  } catch {
    return {};
  }
}

function transcriptItems(
  parts: { seq: bigint; kind: string; payload: Uint8Array; createdAt?: { seconds: bigint } }[] | undefined,
): ChatItem[] {  if (!parts?.length) return [];
  const out: ChatItem[] = [];
  // Phase counter: each opencode step is one assistant generation period.
  // step_start / step_finish parts are boundaries that close the phase but
  // render nothing; every text/reasoning part is tagged with the step it
  // belongs to. The keys are derived from seq order, so they are stable
  // across the 2s transcript-flush renders (no mid-stream regrouping).
  let step = 0;
  for (const p of parts) {
    const at = p.createdAt?.seconds
      ? Number(p.createdAt.seconds) * 1000
      : Date.now();
    const key = `t-${p.seq.toString()}`;
    const pl = decodePayload(p.payload);
    switch (p.kind) {
      case "step_start":
      case "step_finish":
        step++;
        break;
      case "user_message":
        if (typeof pl.text === "string" && pl.text) {
          out.push({
            kind: "user",
            text: pl.text,
            source: pl.source || "goal",
            at,
            key,
          });
        }
        break;
      case "text":
        if (typeof pl.part?.text === "string" && pl.part.text) {
          out.push({ kind: "text", text: pl.part.text, at, key, phase: `step-${step}` });
        }
        break;
      case "tool_use": {
        const part = pl.part || {};
        const state = part.state || {};
        out.push({
          kind: "tool",
          tool: {
            id: key,
            toolName: part.tool || "tool",
            input:
              typeof state.input === "string"
                ? state.input
                : JSON.stringify(state.input ?? ""),
            output:
              typeof state.output === "string" ? state.output : "",
            at,
          },
          key,
        });
        break;
      }
      case "reasoning":
        if (typeof pl.part?.text === "string" && pl.part.text) {
          out.push({ kind: "reasoning", text: pl.part.text, at, key, phase: `step-${step}` });
        }
        break;
      case "session_info":
        if (typeof pl.session_id === "string" && pl.session_id) {
          out.push({
            kind: "session",
            sessionId: pl.session_id,
            serveUrl: typeof pl.serve_url === "string" ? pl.serve_url : "",
            at,
            key,
          });
        }
        break;
      case "error":
        out.push({ kind: "error", text: pl.error?.message || pl.error?.name || "opencode session error", at, key });
        break;
      default:
        break;
    }
  }
  // Group the history per (kind, phase) so the terminal view — which
  // renders the transcript alone — converges with the live view: each
  // step's per-chunk reasoning parts become ONE bubble.
  return groupByPhase(out);
}

// --- live event stream → chat items (mirrors RuntimeSessionPane) --------

function liveItems(events: StreamExecutionEventsResponse[]): ChatItem[] {
  const out: ChatItem[] = [];
  // Synthetic phase counter: the live stream has no step markers, so we
  // increment on the boundary events (tool / error) that separate one
  // assistant generation period from the next. The events array is
  // monotonic, so these `live-N` keys are stable across renders.
  let phase = 0;
  for (const resp of events) {
    const evt = resp.event;
    if (!evt) continue;
    const at = evt.occurredAt ? Number(evt.occurredAt.seconds) * 1000 : Date.now();
    const id = evt.eventId || `${resp.sequence}`;
    const payload = decodePayload(evt.payload);
    switch (evt.eventType) {
      case 2: {
        // TELEMETRY — assistant text or reasoning chunk. Reasoning chunks
        // arrive wrapped as {"kind":"reasoning","text":"...","seq":N} (the
        // adapter's emitReasoningChunked); detect by parsing the payload,
        // not a top-level kind field.
        const raw = payload.text as string | undefined;
        if (typeof raw !== "string" || !raw.length) break;
        let text = raw;
        let isReasoning = false;
        if (raw.startsWith("{")) {
          try {
            const parsed = JSON.parse(raw);
            if (parsed && parsed.kind === "reasoning" && typeof parsed.text === "string") {
              isReasoning = true;
              text = parsed.text;
            }
          } catch {
            /* not JSON — treat as plain text */
          }
        }
        out.push(
          isReasoning
            ? { kind: "reasoning", text, at, key: `r-${id}`, live: true, phase: `live-${phase}` }
            : { kind: "text", text, at, key: id, live: true, phase: `live-${phase}` },
        );
        break;
      }
      case 3: {
        const toolName = payload.tool_name || "tool";
        const input = (payload.input as string) || "";
        const output = (payload.output as string) || "";
        out.push({ kind: "tool", tool: { id, toolName, input, output, at }, key: id });
        phase++;
        break;
      }
      case 8:
        out.push({
          kind: "error",
          text: (payload.text as string) || payload.message || "execution error",
          at,
          key: id,
        });
        phase++;
        break;
      case 10: {
        // ARTIFACT
        const name = (payload.artifact_name as string) || "artifact";
        out.push({
          kind: "artifact",
          name,
          type: (payload.artifact_type as string) || "text",
          content: (payload.content as string) || "",
          at,
          key: id,
        });
        break;
      }
      default:
        break;
    }
  }
  return out;
}

// --- bubble components ---------------------------------------------------

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      aria-label="Copy"
      className="rounded p-1 text-muted-foreground/60 transition-colors hover:bg-accent hover:text-foreground"
      onClick={() => {
        navigator.clipboard?.writeText(text).catch(() => {});
        setCopied(true);
        window.setTimeout(() => setCopied(false), 1200);
      }}
    >
      {copied ? (
        <span className="text-[10px] font-medium text-emerald-500">copied</span>
      ) : (
        <Copy className="h-3 w-3" />
      )}
    </button>
  );
}

function SystemPromptBubble({ text }: { text: string }) {
  // The first bubble IS the full prompt sent to the worker — right-aligned
  // like our message to the worker. A NEUTRAL surface (not primary) so
  // every markdown element renders with normal foreground colors on both
  // themes. Collapsible when long.
  const [open, setOpen] = useState(text.length <= 600);
  const long = text.length > 600;
  return (
    <div className="flex justify-end">
      <div className="max-w-[92%] overflow-hidden rounded-2xl rounded-br-sm border border-amber-300/40 bg-amber-50/30 shadow-sm dark:border-amber-500/30 dark:bg-amber-500/10">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="flex w-full items-center gap-2 px-4 py-2 text-left"
        >
          <span className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-amber-500" />
          <span className="text-xs font-medium uppercase tracking-wide text-amber-700 dark:text-amber-300">
            system prompt
          </span>
          <span className="ml-auto shrink-0 text-xs text-muted-foreground/70">
            {text.length.toLocaleString()} chars
          </span>
          {long && (
            <span className="shrink-0 text-xs text-muted-foreground/70">
              {open ? "collapse" : "expand"}
            </span>
          )}
        </button>
        {open && (
          <div className="border-t border-border/40 px-4 py-3">
            <div className="break-words text-sm leading-relaxed [overflow-wrap:anywhere]">
              <Markdown>{text}</Markdown>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

function UserBubble({ text, source }: { text: string; source: string }) {
  const label =
    source === "nudge"
      ? "liveness check"
      : source === "human" || source === "follow_up"
        ? "you"
        : "goal";
  return (
    <div className="flex justify-end">
      <div className="max-w-[85%] rounded-2xl rounded-br-sm bg-primary px-4 py-2.5 text-sm text-primary-foreground shadow-sm">
        <div className="mb-0.5 flex items-center justify-end gap-2">
          <span className="text-[10px] font-medium uppercase tracking-wide text-primary-foreground/70">
            {label}
          </span>
        </div>
        <div className="break-words [overflow-wrap:anywhere]">
          <Markdown>{text}</Markdown>
        </div>
      </div>
    </div>
  );
}

function AssistantBubble({ text, at, workerName }: { text: string; at: number; workerName?: string }) {
  const [open, setOpen] = useState(true);
  const [raw, setRaw] = useState(false);
  const long = text.length > 900;
  return (
    <div className="flex justify-start">
      <div
        className={cn(
          "max-w-[88%] overflow-hidden rounded-2xl rounded-tl-sm border shadow-sm",
          "border-sky-300/30 bg-sky-50/20 dark:border-sky-950/40 dark:bg-sky-950/10",
        )}
      >
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="flex w-full items-center gap-2 px-4 py-2 text-left"
        >
          <span className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-sky-500" />
          <span className="truncate text-xs font-medium uppercase tracking-wide text-muted-foreground">
            {workerName || "worker"}
          </span>
          <span className="shrink-0 text-xs text-muted-foreground/60">
            {new Date(at).toLocaleTimeString()}
          </span>
          <span className="ml-auto shrink-0 text-xs text-muted-foreground/60">
            {text.length.toLocaleString()} chars
          </span>
          <span className="shrink-0 text-xs text-muted-foreground/60">
            {open ? "collapse" : "expand"}
          </span>
        </button>
        {open && (
          <div className="border-t border-border/40 px-4 py-3">
            <div className="mb-1 flex items-center justify-end gap-2">
              <button
                type="button"
                className="rounded px-1.5 py-0.5 text-xs font-medium text-muted-foreground hover:bg-accent hover:text-accent-foreground"
                onClick={() => setRaw((v) => !v)}
              >
                {raw ? "Render markdown" : "Raw text"}
              </button>
              <CopyButton text={text} />
            </div>
            <div className="break-words text-sm leading-relaxed [overflow-wrap:anywhere]">
              {raw ? (
                <pre className="whitespace-pre-wrap font-mono text-[13px] leading-relaxed">{text}</pre>
              ) : (
                <Markdown>{text}</Markdown>
              )}
            </div>
          </div>
        )}
        {long && !open && (
          <div className="px-4 pb-2 text-xs text-muted-foreground/60">
            {text.length.toLocaleString()} chars — click to expand
          </div>
        )}
      </div>
    </div>
  );
}

function ToolCard({ tool }: { tool: ParsedTool }) {
  const [open, setOpen] = useState(false);
  const hasOutput = tool.output.length > 0;
  const [copyState, setCopyState] = useState<"" | "input" | "output">("");
  const copy = (kind: "input" | "output") => {
    navigator.clipboard?.writeText(kind === "input" ? tool.input : tool.output).catch(() => {});
    setCopyState(kind);
    window.setTimeout(() => setCopyState(""), 1200);
  };
  return (
    <div className="flex justify-start pl-2">
      <div className="w-full max-w-[92%] overflow-hidden rounded-xl border border-border/70 bg-muted/30">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="flex w-full items-center gap-2 px-3 py-2 text-left"
        >
          <TerminalSquare className="h-4 w-4 shrink-0 text-muted-foreground" />
          <span className="font-mono text-sm font-medium">{tool.toolName}</span>
          {hasOutput && (
            <span className="ml-auto shrink-0 rounded bg-muted px-2 py-0.5 text-xs text-muted-foreground">
              {tool.output.length.toLocaleString()} bytes
            </span>
          )}
          <span className="text-xs text-muted-foreground/60">
            {open ? "hide" : "view"}
          </span>
        </button>
        {open && (
          <div className="space-y-2 border-t border-border/50 p-3">
            <div>
              <div className="mb-1 flex items-center gap-2">
                <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">input</span>
                <button
                  type="button"
                  onClick={() => copy("input")}
                  className="ml-auto text-xs text-muted-foreground hover:text-foreground"
                >
                  {copyState === "input" ? "copied" : "copy"}
                </button>
              </div>
              <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-words rounded bg-background/70 p-2 font-mono text-[13px] leading-relaxed">
                {tool.input || "—"}
              </pre>
            </div>
            {hasOutput && (
              <div>
                <div className="mb-1 flex items-center gap-2">
                  <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">output</span>
                  <button
                    type="button"
                    onClick={() => copy("output")}
                    className="ml-auto text-xs text-muted-foreground hover:text-foreground"
                  >
                    {copyState === "output" ? "copied" : "copy"}
                  </button>
                </div>
                <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded bg-background/70 p-2 font-mono text-[13px] leading-relaxed">
                  {tool.output}
                </pre>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

function ArtifactCard({ artifact }: { artifact: { name: string; type: string; content: string } }) {
  const fileName = artifact.name.split("/").pop() || artifact.name;
  const isMarkdown = artifact.type === "markdown" || fileName.endsWith(".md");
  const [open, setOpen] = useState(false);
  return (
    <div className="flex justify-start pl-2">
      <div className="w-full max-w-[92%] overflow-hidden rounded-xl border border-sky-300/40 bg-sky-50/30 dark:bg-sky-950/20">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="flex w-full items-center gap-2 px-3 py-2 text-left"
        >
          <span className="rounded bg-sky-200 px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wide dark:bg-sky-900">
            artifact
          </span>
          <span className="truncate font-mono text-xs">{fileName}</span>
          <span className="ml-auto shrink-0 text-[10px] text-muted-foreground">
            {artifact.content.length.toLocaleString()} bytes
          </span>
        </button>
        {open && (
          <div className="border-t border-border/50 p-3">
            {isMarkdown ? (
              <div className="max-h-48 overflow-auto rounded bg-background/70 p-2 text-xs leading-relaxed">
                <Markdown>{artifact.content}</Markdown>
              </div>
            ) : (
              <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded bg-background/70 p-2 font-mono text-[11px] leading-relaxed">
                {artifact.content}
              </pre>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

function ErrorBubble({ text }: { text: string }) {
  return (
    <div className="flex justify-start pl-2">
      <div className="max-w-[92%] rounded-xl border border-destructive/50 bg-destructive/10 px-3 py-2 text-xs text-destructive">
        {text}
      </div>
    </div>
  );
}

// --- main pane -----------------------------------------------------------

export function SessionChatPane({
  executionId,
  events,
  streamStatus,
  storedOutput,
  workerName,
  systemPrompt,
  isRunning,
  isTerminal,
}: SessionChatPaneProps) {
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const stickRef = useRef(true);
  const [showJump, setShowJump] = useState(false);
  const [composerOpen, setComposerOpen] = useState(false);
  const [draft, setDraft] = useState("");
  // Optimistic follow-up bubbles: on a completed execution, the user's
  // question is shown immediately while the model processes, before the
  // server records it into the durable transcript.
  const [pending, setPending] = useState<{ key: string; text: string; at: number }[]>([]);
  const pendingSeq = useRef(0);

  const sendMsg = useSendExecutionMessage();
  const continueSession = useContinueExecutionSession();
  const { data: transcript, refetch: refetchTranscript } = useGetExecutionSession(executionId, true);
  // A follow-up on a completed execution is fire-and-forget: the RPC returns
  // immediately and the reply lands in the transcript asynchronously. Track
  // that the reply is pending so the pane keeps polling until it arrives.
  const [followUpReplyPending, setFollowUpReplyPending] = useState(false);
  // Poll every 2s while running so the pane shows the full conversation as
  // it grows (joining mid-run shows everything already said; the runner
  // flushes the transcript every ~2s).
  useEffect(() => {
    if (!isRunning) return;
    const t = window.setInterval(() => {
      void refetchTranscript();
    }, 2000);
    return () => window.clearInterval(t);
  }, [isRunning, refetchTranscript]);

  // When the execution finishes, the runner does a final transcript flush —
  // refetch so the last message appears without a manual refresh.
  const wasRunning = useRef(isRunning);
  useEffect(() => {
    if (wasRunning.current && !isRunning) {
      void refetchTranscript();
    }
    wasRunning.current = isRunning;
  }, [isRunning, refetchTranscript]);

  // A completed-execution follow-up is fire-and-forget: the RPC returns at
  // once and the reply is written to the transcript later. While a follow-up
  // reply is pending, poll the transcript so the assistant's reply appears
  // without a manual refresh (the model can take minutes).
  const followUpReplySeq = useRef(0n);
  useEffect(() => {
    if (!followUpReplyPending) return;
    const parts = transcript ?? [];
    // The reply is the assistant text that lands AFTER the follow-up was
    // sent. The boundary is fixed at send time (the max transcript seq then)
    // so a previous follow-up's reply in an ongoing conversation can never be
    // mistaken for this one's. If the transcript wasn't loaded at send time
    // (boundary 0n), establish it lazily from the newest follow_up user
    // message once it appears.
    let boundary = followUpReplySeq.current;
    if (boundary === 0n) {
      let latestFollowUp = 0n;
      for (const p of parts) {
        if (p.kind !== "user_message") continue;
        const pl = decodePayload(p.payload);
        if (pl.source === "follow_up" && p.seq > latestFollowUp) {
          latestFollowUp = p.seq;
        }
      }
      if (latestFollowUp > 0n) {
        followUpReplySeq.current = latestFollowUp;
        boundary = latestFollowUp;
      }
    }
    const replied =
      boundary > 0n &&
      parts.some((p) => p.kind === "text" && p.seq > boundary);
    if (replied) {
      setFollowUpReplyPending(false);
      return;
    }
    const t = window.setInterval(() => void refetchTranscript(), 2000);
    return () => window.clearInterval(t);
  }, [followUpReplyPending, transcript, refetchTranscript]);
  // Poll while running so user messages (goal/nudges/human) appear even
  // before the terminal flush.
  const live = useMemo(() => liveItems(events), [events]);
  const history = useMemo(() => transcriptItems(transcript), [transcript]);

  // The full system prompt: prefer the page's enriched value, fall back to
  // the transcript's recorded system_prompt part (reliable after reload).
  const systemText = useMemo(() => {
    if (systemPrompt) return systemPrompt;
    const sp = transcript?.find((p) => p.kind === "system_prompt");
    if (!sp) return undefined;
    return decodePayload(sp.payload)?.text as string | undefined;
  }, [systemPrompt, transcript]);

  // Optimistic follow-ups: any pending bubble whose text the durable
  // transcript has already recorded as a follow_up user message is a
  // duplicate — drop it (the transcript's own bubble takes over).
  const sentFollowUpTexts = useMemo(() => {
    const set = new Set<string>();
    for (const p of transcript ?? []) {
      if (p.kind !== "user_message") continue;
      const pl = decodePayload(p.payload);
      if (typeof pl.text === "string" && pl.text) set.add(pl.text);
    }
    return set;
  }, [transcript]);
  useEffect(() => {
    if (pending.length === 0) return;
    setPending((prev) => {
      const next = prev.filter((p) => !sentFollowUpTexts.has(p.text));
      return next.length === prev.length ? prev : next;
    });
  }, [pending, sentFollowUpTexts]);
  const pendingItems = useMemo(
    () => pending.filter((p) => !sentFollowUpTexts.has(p.text)),
    [pending, sentFollowUpTexts],
  );

  // Merge: running → the FULL transcript history (so joining mid-run shows
  // everything already said) plus only the live-stream events that are NEWER
  // than the transcript's latest part (the runner flushes every ~2s, so the
  // live stream fills the gap until the next flush) — consecutive live text
  // chunks are grouped into one growing assistant bubble. Terminal → the
  // transcript alone (durable, includes both sides).
  const items = useMemo<ChatItem[]>(() => {
    if (isRunning) {
      return mergeSessionItems(history, live);
    }
    if (history.length > 0) return history;
    // No session transcript (legacy execution / one-shot fallback): show
    // the stored output as an assistant message.
    if (storedOutput) {
      return [{ kind: "text", text: storedOutput, at: Date.now(), key: "stored" }];
    }
    return live;
  }, [isRunning, history, live, storedOutput]);

  // Auto-stick scroll: track whether the user is near the bottom; if so,
  // keep it pinned on new items. The stick flag must only clear on a REAL
  // manual scroll — the auto-scroll's own scroll event (and the smooth
  // "jump to latest" animation) would otherwise reset it mid-stream and
  // stop the view from following the conversation.
  const lastAutoScrollRef = useRef(0);
  const onScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    if (Date.now() - lastAutoScrollRef.current < 200) return; // our own scroll
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    stickRef.current = nearBottom;
    setShowJump(!nearBottom);
  }, []);

  useEffect(() => {
    if (stickRef.current && scrollRef.current) {
      // Instant jump (not smooth) so rapid updates always land exactly at
      // the bottom; mark the timestamp so onScroll ignores this event.
      lastAutoScrollRef.current = Date.now();
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [items, live, pendingItems]);

  const jumpToBottom = useCallback(() => {
    stickRef.current = true;
    setShowJump(false);
    lastAutoScrollRef.current = Date.now();
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight, behavior: "smooth" });
  }, []);

  const handleSend = useCallback(
    (text: string) => {
      const msg = text.trim();
      if (!msg) return;
      if (isRunning) {
        sendMsg.mutate(
          { executionId, message: msg },
          {
            onSuccess: () => {
              setDraft("");
              setComposerOpen(false);
            },
          },
        );
        return;
      }
      // Completed execution: run the follow-up IN the session (no new
      // execution/work item). The RPC returns immediately (fire-and-forget);
      // the reply is recorded into the transcript asynchronously. Show the
      // user's bubble immediately, mark the reply as pending, and poll until
      // the durable transcript carries the assistant's reply. The boundary is
      // the max transcript seq AT SEND TIME — the reply lands after it.
      const key = `pending-${pendingSeq.current++}`;
      setPending((prev) => [...prev, { key, text: msg, at: Date.now() }]);
      setDraft("");
      let boundary = 0n;
      for (const p of transcript ?? []) {
        if (p.seq > boundary) boundary = p.seq;
      }
      followUpReplySeq.current = boundary;
      setFollowUpReplyPending(true);
      continueSession.mutate(
        { executionId, message: msg },
        {
          onSuccess: () => {
            setComposerOpen(false);
          },
          onError: () => {
            setFollowUpReplyPending(false);
          },
          onSettled: () => {
            void refetchTranscript();
          },
        },
      );
    },
    [executionId, isRunning, sendMsg, continueSession, refetchTranscript, transcript],
  );

  const isStreaming = streamStatus === "open" && isRunning;
  const followUpPending =
    !isRunning && (continueSession.isPending || followUpReplyPending);
  const busy = sendMsg.isPending || continueSession.isPending;

  // The system-prompt bubble above already carries the full composite
  // (which includes the goal); drop the redundant standalone goal bubble
  // from the visible list when the prompt is shown. Optimistic follow-up
  // bubbles append after the transcript so they show while the model works.
  const visibleItems = useMemo(
    () => {
      const base = systemText
        ? items.filter((i) => !(i.kind === "user" && i.source === "goal"))
        : items;
      if (pendingItems.length === 0) return base;
      return [
        ...base,
        ...pendingItems.map((p) => ({
          kind: "user" as const,
          text: p.text,
          source: "follow_up",
          at: p.at,
          key: p.key,
        })),
      ];
    },
    [items, systemText, pendingItems],
  );

  return (
    <div className="flex h-[calc(100vh-260px)] min-h-[420px] flex-col overflow-hidden rounded-2xl border border-border bg-card/50 shadow-sm">
      {/* header */}
      <div className="flex items-center gap-2 border-b border-border/60 px-4 py-2.5">
        <span
          className={cn(
            "inline-block h-2 w-2 rounded-full",
            isStreaming ? "animate-pulse bg-emerald-500" : "bg-muted-foreground",
          )}
        />
        <span className="text-sm font-medium">Session</span>
        <span className="text-xs text-muted-foreground">
          {isStreaming ? "live" : isTerminal ? "completed" : streamStatus || "idle"}
        </span>
        <div className="ml-auto flex items-center gap-2 text-xs text-muted-foreground">
          <span>{visibleItems.length + (systemText ? 1 : 0)} message{visibleItems.length + (systemText ? 1 : 0) === 1 ? "" : "s"}</span>
        </div>
      </div>

      {/* scroll column */}
      <div
        ref={scrollRef}
        onScroll={onScroll}
        className="relative flex-1 space-y-3 overflow-y-auto px-4 py-4"
      >
        {systemText && <SystemPromptBubble text={systemText} />}

        {visibleItems.length === 0 && !systemText && (
          <p className="py-10 text-center text-sm text-muted-foreground">
            {isStreaming
              ? "Waiting for model output…"
              : streamStatus === "connecting" || streamStatus === "reconnecting"
                ? "Connecting to event stream…"
                : "No session activity yet."}
          </p>
        )}

        {visibleItems.map((item) => {
          switch (item.kind) {
            case "user":
              return <UserBubble key={item.key} text={item.text} source={item.source} />;
            case "text":
              return <AssistantBubble key={item.key} text={item.text} at={item.at} workerName={workerName} />;
            case "tool":
              return <ToolCard key={item.key} tool={item.tool} />;
            case "reasoning":
              return <ReasoningBubble key={item.key} text={item.text} streaming={!!item.live} />;
            case "artifact":
              return <ArtifactCard key={item.key} artifact={item} />;
            case "error":
              return <ErrorBubble key={item.key} text={item.text} />;
            case "session":
              return (
                <div key={item.key} className="flex justify-center">
                  <span className="inline-flex max-w-full items-center gap-1.5 truncate rounded-full border border-border bg-muted/40 px-2.5 py-1 font-mono text-[10px] text-muted-foreground">
                    <span className="inline-block h-1.5 w-1.5 rounded-full bg-emerald-500" />
                    <span className="truncate">{item.sessionId}</span>
                    {item.serveUrl && <span className="opacity-60">{item.serveUrl}</span>}
                  </span>
                </div>
              );
          }
        })}

        {(isStreaming || followUpPending) && visibleItems.length > 0 && (
          <div className="flex items-center gap-2 pl-2 text-xs text-muted-foreground">
            <span className="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-emerald-500" />
            {isRunning ? "worker is thinking…" : "worker is responding…"}
          </div>
        )}

        {showJump && (
          <button
            type="button"
            onClick={jumpToBottom}
            className="sticky bottom-2 z-10 mx-auto flex items-center gap-1.5 rounded-full border border-border bg-background px-3 py-1.5 text-xs shadow-md hover:bg-accent"
          >
            <ArrowDown className="h-3 w-3" />
            jump to latest
          </button>
        )}
      </div>

      {/* composer — always available: nudge the live session mid-run, or
          run a one-shot follow-up IN the session when the execution is
          terminal (no new execution/work item). */}
      <div className="border-t border-border/60 p-3">
        {composerOpen ? (
          <div className="flex items-end gap-2">
            <textarea
              autoFocus
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault();
                  handleSend(draft);
                }
              }}
              placeholder={
                isRunning
                  ? "Message the worker mid-run (no new work item is created)…"
                  : "Ask a follow-up — it continues this conversation…"
              }
              className="max-h-32 min-h-[40px] flex-1 resize-none rounded-xl border border-border bg-background px-3 py-2 text-sm outline-none focus:border-primary"
            />
            <Button size="sm" onClick={() => handleSend(draft)} disabled={!draft.trim() || busy}>
              <SendHorizontal className="h-4 w-4" />
            </Button>
            <Button
              size="sm"
              variant="destructive"
              onClick={() => {
                setComposerOpen(false);
                setDraft("");
              }}
              title="Discard"
            >
              <Square className="h-3 w-3" />
            </Button>
          </div>
        ) : (
          <button
            type="button"
            onClick={() => setComposerOpen(true)}
            className="w-full rounded-xl border border-dashed border-border bg-background/60 px-3 py-2 text-left text-sm text-muted-foreground hover:border-primary/50 hover:text-foreground"
          >
            {isRunning
              ? "Nudge the worker — send a message into the live session…"
              : "Continue the conversation — ask a follow-up…"}
          </button>
        )}
        {(sendMsg.isError || continueSession.isError) && (
          <p className="mt-2 text-xs text-destructive">
            Could not send: {String((sendMsg.error ?? continueSession.error)?.message ?? sendMsg.error ?? continueSession.error)}
          </p>
        )}
      </div>
    </div>
  );
}
