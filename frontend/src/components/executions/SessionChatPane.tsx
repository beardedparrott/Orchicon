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
  Loader2,
  SendHorizontal,
  Square,
  TerminalSquare,
} from "lucide-react";
import type { StreamExecutionEventsResponse } from "@/api/gen/orchicon/api/v1/execution_pb";
import { useGetExecutionSession, useSendExecutionMessage, useContinueExecutionSession } from "@/api/executions";
import { Markdown } from "@/components/markdown";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

interface ParsedTool {
  id: string;
  toolName: string;
  input: string;
  output: string;
  at: number;
}

type ChatItem =
  | { kind: "user"; text: string; source: string; at: number; key: string }
  | { kind: "text"; text: string; at: number; key: string }
  | { kind: "tool"; tool: ParsedTool; key: string }
  | { kind: "reasoning"; text: string; at: number; key: string }
  | { kind: "error"; text: string; at: number; key: string }
  | { kind: "artifact"; name: string; type: string; content: string; at: number; key: string }
  | { kind: "session"; sessionId: string; serveUrl: string; at: number; key: string };

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
  for (const p of parts) {
    const at = p.createdAt?.seconds
      ? Number(p.createdAt.seconds) * 1000
      : Date.now();
    const key = `t-${p.seq.toString()}`;
    const pl = decodePayload(p.payload);
    switch (p.kind) {
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
          out.push({ kind: "text", text: pl.part.text, at, key });
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
          out.push({ kind: "reasoning", text: pl.part.text, at, key });
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
  return out;
}

// --- live event stream → chat items (mirrors RuntimeSessionPane) --------

function liveItems(events: StreamExecutionEventsResponse[]): ChatItem[] {
  const out: ChatItem[] = [];
  for (const resp of events) {
    const evt = resp.event;
    if (!evt) continue;
    const at = evt.occurredAt ? Number(evt.occurredAt.seconds) * 1000 : Date.now();
    const id = evt.eventId || `${resp.sequence}`;
    const payload = decodePayload(evt.payload);
    switch (evt.eventType) {
      case 2: {
        // TELEMETRY — assistant text or reasoning chunk.
        const raw = payload.text as string | undefined;
        if (typeof raw !== "string" || !raw.length) break;
        const isReasoning = payload.kind === "reasoning" && raw.startsWith("{");
        let text = raw;
        if (isReasoning) {
          try {
            const parsed = JSON.parse(raw);
            if (parsed && typeof parsed.text === "string") text = parsed.text;
          } catch {
            /* fall through as plain text */
          }
        }
        out.push(
          isReasoning
            ? { kind: "reasoning", text, at, key: `r-${id}` }
            : { kind: "text", text, at, key: id },
        );
        break;
      }
      case 3: {
        const toolName = payload.tool_name || "tool";
        const input = (payload.input as string) || "";
        const output = (payload.output as string) || "";
        out.push({ kind: "tool", tool: { id, toolName, input, output, at }, key: id });
        break;
      }
      case 8:
        out.push({
          kind: "error",
          text: (payload.text as string) || payload.message || "execution error",
          at,
          key: id,
        });
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
  // The first bubble IS the full prompt sent to the worker — styled like a
  // user message (we sent it), collapsible only when long.
  const [open, setOpen] = useState(text.length <= 400);
  const long = text.length > 400;
  return (
    <div className="flex justify-end">
      <div className="max-w-[88%] overflow-hidden rounded-2xl rounded-br-sm bg-primary px-4 py-2.5 text-sm text-primary-foreground shadow-sm">
        <div className="mb-1 flex items-center justify-end gap-2">
          <span className="text-[10px] font-medium uppercase tracking-wide text-primary-foreground/70">
            system prompt
          </span>
          <span className="text-[10px] opacity-60">
            {text.length.toLocaleString()} chars
          </span>
          {long && (
            <button
              type="button"
              onClick={() => setOpen((v) => !v)}
              className="rounded px-1.5 py-0.5 text-[10px] font-medium text-primary-foreground/80 hover:bg-primary-foreground/10"
            >
              {open ? "collapse" : "expand"}
            </button>
          )}
        </div>
        {open && (
          <div className="break-words text-sm leading-relaxed [overflow-wrap:anywhere]">
            <Markdown>{text}</Markdown>
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

function ReasoningBubble({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="flex justify-start pl-2">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="max-w-[92%] rounded-xl border border-violet-300/30 bg-violet-50/20 px-3 py-2 text-left text-[13px] italic leading-relaxed text-muted-foreground dark:bg-violet-950/10"
      >
        <span className="flex items-center gap-1.5 font-medium not-italic text-violet-700 dark:text-violet-300">
          <span className="inline-block h-1.5 w-1.5 rounded-full bg-violet-500" />
          reasoning {open ? "· hide" : `· ${text.length.toLocaleString()} chars`}
        </span>
        {open && (
          <span className="mt-1 block whitespace-pre-wrap break-words">{text}</span>
        )}
      </button>
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

  const sendMsg = useSendExecutionMessage();
  const continueSession = useContinueExecutionSession();
  const { data: transcript, isFetching: transcriptLoading } = useGetExecutionSession(
    executionId,
    true,
  );
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

  // Merge: running → live items + user messages from the transcript;
  // terminal → the transcript alone (durable, includes both sides).
  const items = useMemo<ChatItem[]>(() => {
    const atOf = (i: ChatItem): number => (i.kind === "tool" ? i.tool.at : i.at);
    if (isRunning) {
      const extras = history.filter((i) => i.kind === "user" || i.kind === "session");
      return [...extras, ...live].sort((a, b) => atOf(a) - atOf(b));
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
  // keep it pinned on new items.
  const onScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    stickRef.current = nearBottom;
    setShowJump(!nearBottom);
  }, []);

  useEffect(() => {
    if (stickRef.current) {
      scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight, behavior: "smooth" });
    }
  }, [items, live]);

  const jumpToBottom = useCallback(() => {
    stickRef.current = true;
    setShowJump(false);
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
      // execution/work item); the reply lands in the transcript and the
      // session query refresh renders it inline in this chat.
      continueSession.mutate(
        { executionId, message: msg },
        {
          onSuccess: () => {
            setDraft("");
            setComposerOpen(false);
          },
        },
      );
    },
    [executionId, isRunning, sendMsg, continueSession],
  );

  const isStreaming = streamStatus === "open" && isRunning;
  const followUpPending = !isRunning && continueSession.isPending;
  const busy = sendMsg.isPending || continueSession.isPending;

  // The system-prompt bubble above already carries the full composite
  // (which includes the goal); drop the redundant standalone goal bubble
  // from the visible list when the prompt is shown.
  const visibleItems = useMemo(
    () =>
      systemText
        ? items.filter((i) => !(i.kind === "user" && i.source === "goal"))
        : items,
    [items, systemText],
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
        {transcriptLoading && isRunning && <Loader2 className="h-3 w-3 animate-spin text-muted-foreground" />}
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
              return <ReasoningBubble key={item.key} text={item.text} />;
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
