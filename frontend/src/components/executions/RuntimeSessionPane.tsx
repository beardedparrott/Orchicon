// RuntimeSessionPane — live opencode-style chat view of an execution.
//
// The user feedback was clear: the previous tabbed Prompt/Output/Tool
// Calls layout buried the streaming model output and made it hard to
// follow what opencode is doing. This pane instead renders the
// execution's events in chronological order as a chat thread:
//
//   ┌─ System prompt (collapsed by default) ──────────────────────┐
//   │ System prompt text                                          │
//   └─────────────────────────────────────────────────────────────┘
//
//   ┌─ Tool call: bash ───────────────────────────────────────────┐
//   │ Input:  {"command": "ls -la"}                                │
//   │ Result: total 42 ...                                         │
//   └─────────────────────────────────────────────────────────────┘
//
//   ┌─ Assistant ──────────────────────────────────────────────────┐
//   │ [text output as it streams in — chat-bubble style]            │
//   └─────────────────────────────────────────────────────────────┘
//
// The pane consumes the same StreamExecutionEvents stream as the rest
// of the page; the useStream hook gives us a monotonically growing
// event array, so the chat scrolls naturally as new events arrive.
//
// "Subagent" tool calls (opencode emits them as tool_call events when
// the parent invokes the `task` tool to spawn a child session) show
// up here the same way — they're just another tool_call with a tool
// name like `task` and a JSON input that includes the prompt sent to
// the child. There is no separate "subagent" event type; subagent
// visibility is the same as tool-call visibility, which is the point.
import { useEffect, useMemo, useRef } from "react";
import type { StreamExecutionEventsResponse } from "@/api/gen/orchicon/api/v1/execution_pb";
import { Markdown } from "@/components/markdown";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";

interface ParsedToolCall {
  id: string;
  toolName: string;
  input: string;
  output: string;
  occurredAt: Date;
}

interface ParsedTextChunk {
  id: string;
  text: string;
  occurredAt: Date;
}

interface ParsedReasoningChunk {
  id: string;
  text: string;
  occurredAt: Date;
}

interface ParsedResult {
  text: string;
  occurredAt: Date;
}

interface ParsedError {
  message: string;
  occurredAt: Date;
}

interface ParsedArtifact {
  name: string;
  artifactType: string;
  content: string;
  occurredAt: Date;
}

// ChatMessage is the unified type the renderer iterates over. The
// streaming array of events is collapsed into an ordered list of
// messages so the timeline reads top-to-bottom like a chat log.
type ChatMessage =
  | { kind: "prompt"; text: string; key: string }
  | { kind: "system"; text: string; key: string; occurredAt: Date }
  | { kind: "tool"; tool: ParsedToolCall; key: string }
  | { kind: "text"; chunk: ParsedTextChunk; key: string }
  | { kind: "reasoning"; chunk: ParsedReasoningChunk; key: string }
  | { kind: "result"; result: ParsedResult; key: string }
  | { kind: "error"; error: ParsedError; key: string }
  | { kind: "artifact"; artifact: ParsedArtifact; key: string };

interface RuntimeSessionPaneProps {
  events: StreamExecutionEventsResponse[];
  prompt?: string;
  /** Connection status of the underlying event stream (used for the
   *  "live" indicator). "open" = connected, anything else = degraded. */
  streamStatus?: string;
  /** Stored output from the execution record — shown when no live stream
   *  events are available (e.g. after navigating back to a completed
   *  execution). This ensures the model's text output survives page
   *  navigation (docs/02 §2.7). */
  storedOutput?: string;
}

export function RuntimeSessionPane({ events, prompt, streamStatus, storedOutput }: RuntimeSessionPaneProps) {
  const scrollRef = useRef<HTMLDivElement | null>(null);

  // Build the chronological message list. Tool_call events come in
  // pairs (input, then output, often separated by other events), so we
  // merge them by tool name within a small window — the second event
  // fills the `output` slot of the first. Anything that doesn't pair
  // still renders as a tool card with just input (the most common case
  // — opencode emits tool_call immediately and tool_result on
  // completion).
  const messages = useMemo<ChatMessage[]>(() => {
    const out: ChatMessage[] = [];
    // Track tool inputs by tool name so we can pair a later tool_call
    // with output into the same card. Keyed by eventId so we never
    // accidentally merge unrelated consecutive calls of the same
    // tool.
    const openInputs = new Map<string, ParsedToolCall>();

    for (const resp of events) {
      const evt = resp.event;
      if (!evt) continue;
      const ts = evt.occurredAt
        ? new Date(Number(evt.occurredAt.seconds) * 1000)
        : new Date();
      const id = evt.eventId || `${resp.sequence}`;

      // Decode the JSON payload once. Event payloads are
      // orchestration-side envelopes (see enqueueExecEvent in
      // scheduler/reconciler.go) — they include a `tool_name` /
      // `text` / standard fields depending on event_type. The
      // shape is duck-typed on the read side and the source of
      // truth lives in `internal/scheduler/reconciler.go`, so we
      // accept `unknown`-shaped fields and narrow on read.
      /* eslint-disable @typescript-eslint/no-explicit-any */
      let payload: any = {};
      /* eslint-enable @typescript-eslint/no-explicit-any */
      if (evt.payload?.length) {
        try {
          const raw = new TextDecoder().decode(evt.payload);
          payload = JSON.parse(raw);
        } catch {
          // ignore — unparseable payload still renders as a system
          // line so the user sees something arrived
        }
      }

      const eventType = payload.event_type || "";

      // 1 = STARTED, 2 = TELEMETRY (model text OR reasoning chunks),
      // 3 = TOOL_CALL (and results), 7 = RESULT (final), 8 = ERROR.
      // The numeric constants match ExecutionEventType in
      // proto/orchicon/api/v1/execution.proto.
      //
      // Reasoning chunks are tagged at the payload level by the
      // adapter: each chunk is JSON-encoded as
      // `{"kind":"reasoning","text":"...","seq":N}`. We unwrap and
      // route them to a distinct bubble so the operator can see the
      // model's "thinking" alongside the actual response.
      switch (evt.eventType) {
        case 1: // STARTED
          out.push({
            kind: "system",
            text: payload.message || "Execution started",
            key: id,
            occurredAt: ts,
          });
          break;
        case 2: {
          // TELEMETRY — either an assistant text chunk or a
          // reasoning chunk. The adapter tags reasoning with
          // `kind: "reasoning"` so we can route it here.
          const rawText = payload.text;
          if (typeof rawText !== "string" || rawText.length === 0) break;
          // The reasoning wrapper is JSON-encoded inside the text
          // payload. Try to detect and unwrap; if it isn't JSON,
          // treat it as plain text.
          const asText = rawText;
          let asReasoning: string | null = null;
          if (rawText.startsWith("{") && payload.kind === "reasoning") {
            try {
              const parsed = JSON.parse(rawText);
              if (parsed && typeof parsed.text === "string") {
                asReasoning = parsed.text;
              }
            } catch {
              // not JSON — fall through as plain text
            }
          }
          if (asReasoning !== null) {
            out.push({
              kind: "reasoning",
              chunk: {
                id,
                text: asReasoning,
                occurredAt: ts,
              },
              key: `reason-${id}`,
            });
          } else {
            out.push({
              kind: "text",
              chunk: {
                id,
                text: asText,
                occurredAt: ts,
              },
              key: id,
            });
          }
          break;
        }
        case 3: {
          // TOOL_CALL — opencode v1.x emits a single `tool_use` event
          // per tool invocation that carries BOTH input and output
          // (state.input + state.output). Render that as a single
          // complete card. The previous behavior — pairing two
          // separate events — still works for the legacy
          // tool_call/tool_result pair shape: an input-only event
          // creates a pending card, a later output-only event fills
          // it.
          const toolName = payload.tool_name || "tool";
          const input = (payload.input as string) || "";
          const output = (payload.output as string) || "";
          if (input && output) {
            // Complete tool_use event — render as one card with both.
            out.push({
              kind: "tool",
              tool: {
                id,
                toolName,
                input,
                output,
                occurredAt: ts,
              },
              key: id,
            });
          } else if (input && !output) {
            const tc: ParsedToolCall = {
              id,
              toolName,
              input,
              output: "",
              occurredAt: ts,
            };
            openInputs.set(id, tc);
            out.push({ kind: "tool", tool: tc, key: id });
          } else if (output) {
            // The matching input event_id isn't on the wire, so we
            // match by tool name + position. If a tool card without
            // output is the most recent pending tool card with the
            // same name, attach the output to it; otherwise emit a
            // standalone result card.
            let paired = false;
            for (let i = out.length - 1; i >= 0; i--) {
              const m = out[i];
              if (m.kind === "tool" && !m.tool.output && m.tool.toolName === toolName) {
                m.tool.output = output;
                m.tool.occurredAt = ts;
                paired = true;
                break;
              }
            }
            if (!paired) {
              out.push({
                kind: "tool",
                tool: {
                  id,
                  toolName,
                  input: "",
                  output,
                  occurredAt: ts,
                },
                key: id,
              });
            }
            // Clean up any stale entries so the map doesn't grow
            // unbounded across long sessions.
            if (openInputs.size > 64) openInputs.clear();
          }
          break;
        }
        case 7: // RESULT (final aggregated result)
          if (payload.text) {
            out.push({
              kind: "result",
              result: { text: payload.text as string, occurredAt: ts },
              key: id,
            });
          }
          break;
        case 8: // ERROR
          out.push({
            kind: "error",
            error: {
              message: (payload.text as string) || payload.message || "Execution error",
              occurredAt: ts,
            },
            key: id,
          });
          break;
        case 10: // ARTIFACT
          if (payload.content) {
            out.push({
              kind: "artifact",
              artifact: {
                name: payload.artifact_name as string || "artifact",
                artifactType: payload.artifact_type as string || "text",
                content: payload.content as string,
                occurredAt: ts,
              },
              key: id,
            });
          }
          break;
        default:
          // Unknown event type — show as a small system note so the
          // operator sees something arrived.
          if (eventType) {
            out.push({
              kind: "system",
              text: eventType,
              key: id,
              occurredAt: ts,
            });
          }
          break;
      }
    }

    return out;
  }, [events]);

  // Auto-scroll to the bottom as new messages arrive. Only sticks to
  // the bottom if the user is already near it — if they scrolled up
  // to read an earlier tool call, don't yank them back. (Standard
  // chat-scroll pattern.)
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 120;
    if (nearBottom) {
      el.scrollTop = el.scrollHeight;
    }
  }, [messages.length]);

// Tool call grouping: collapse consecutive `text` chunks from the
// same event window so the chat doesn't show a million tiny bubbles
// for streamed output. (Each `text` event is one chunk; we keep
// them separate events so the timeline stays accurate, but render
// them as one visual block.) Same applies to `reasoning` chunks —
// the operator should see one continuous "thinking" block, not a
// hundred tiny cards.
const rendered = useMemo<
  Array<
    | ChatMessage
    | { kind: "text-group"; chunks: ParsedTextChunk[] }
    | { kind: "reasoning-group"; chunks: ParsedReasoningChunk[] }
  >
>(() => {
    const blocks: Array<
      | ChatMessage
      | { kind: "text-group"; chunks: ParsedTextChunk[] }
      | { kind: "reasoning-group"; chunks: ParsedReasoningChunk[] }
    > = [];
    for (const m of messages) {
      if (m.kind === "text") {
        const last = blocks[blocks.length - 1];
        if (last && "kind" in last && last.kind === "text-group") {
          last.chunks.push(m.chunk);
        } else {
          blocks.push({ kind: "text-group", chunks: [m.chunk] });
        }
      } else if (m.kind === "reasoning") {
        const last = blocks[blocks.length - 1];
        if (last && "kind" in last && last.kind === "reasoning-group") {
          last.chunks.push(m.chunk);
        } else {
          blocks.push({ kind: "reasoning-group", chunks: [m.chunk] });
        }
      } else {
        blocks.push(m);
      }
    }
    return blocks;
  }, [messages]);

  // Always render the pane when the stream is or was active. A blank
  // card is confusing — the user should see the "Waiting for model
  // output…" state even before any events arrive (docs/10 §11).
  // Only skip rendering when we have no events, no prompt, AND the
  // stream was never open (e.g. the execution detail page and no
  // streaming RPC has connected).
  if (events.length === 0 && !prompt && streamStatus === "idle") return null;

  const isLive = streamStatus === "open";

  return (
    <Card>
      <CardHeader className="pb-3">
        <div className="flex items-center justify-between">
          <CardTitle className="flex items-center gap-2">
            Runtime session
            <span
              className={cn(
                "inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[10px] font-medium",
                isLive
                  ? "bg-emerald-100 text-emerald-700 dark:bg-emerald-900 dark:text-emerald-300"
                  : "bg-muted text-muted-foreground",
              )}
            >
              <span
                className={cn(
                  "inline-block h-1.5 w-1.5 rounded-full",
                  isLive ? "bg-emerald-500" : "bg-muted-foreground",
                )}
              />
              {isLive ? "live" : streamStatus || "idle"}
            </span>
          </CardTitle>
          <span className="text-xs text-muted-foreground">
            {messages.length} event{messages.length === 1 ? "" : "s"}
          </span>
        </div>
      </CardHeader>
      <CardContent>
        <div
          ref={scrollRef}
          className="max-h-[600px] space-y-3 overflow-auto pr-1"
        >
          {/* System prompt at the top — collapsed by default if it's
              long, since most operators just want to see what came
              back, not the whole instructions. */}
          {prompt && (
            <PromptCard prompt={prompt} />
          )}

          {rendered.map((block, idx) => {
            // The rendered list is a heterogeneous array (text
            // groups, reasoning groups, and individual ChatMessage
            // variants). We branch on shape since `block` doesn't
            // narrow automatically across the `kind in block` check
            // + the union.
            if ("chunks" in block) {
              if (block.kind === "reasoning-group") {
                return (
                  <ReasoningBubble key={`rg-${idx}`} chunks={block.chunks} />
                );
              }
              return (
                <AssistantBubble key={`tg-${idx}`} chunks={block.chunks} />
              );
            }
            const m = block as ChatMessage;
            if (m.kind === "prompt") return null;
            switch (m.kind) {
              case "system":
                return <SystemNote key={m.key} text={m.text} ts={m.occurredAt} />;
              case "tool":
                return <ToolCard key={m.key} tool={m.tool} />;
              case "text":
                return null; // already grouped
              case "reasoning":
                return null; // already grouped
              case "result":
                return <ResultCard key={m.key} result={m.result} />;
              case "error":
                return <ErrorCard key={m.key} error={m.error} />;
              case "artifact":
                return <ArtifactCard key={m.key} artifact={m.artifact} />;
            }
          })}

          {messages.length === 0 && !storedOutput && (
            <p className="text-sm text-muted-foreground">
              {streamStatus === "open"
                ? "Waiting for model output…"
                : streamStatus === "connecting" || streamStatus === "reconnecting"
                  ? "Connecting to event stream…"
                  : "No events yet"}
            </p>
          )}

          {/* Show stored output when no live stream events are available.
              This persists the model's text output across page navigation
              (docs/02 §2.7). */}
          {messages.length === 0 && storedOutput && (
            <div className="flex justify-start">
              <div className="max-w-[85%] rounded-lg rounded-tl-sm border border-border bg-card px-3 py-2 text-sm leading-relaxed shadow-sm">
                <div className="mb-1 flex items-center gap-2 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                  <span>assistant output</span>
                  <span className="opacity-60">(stored)</span>
                </div>
                <Markdown>{storedOutput}</Markdown>
              </div>
            </div>
          )}
        </div>
      </CardContent>
    </Card>
  );
}

// --- bubble components ---------------------------------------------------

function PromptCard({ prompt }: { prompt: string }) {
  // Long system prompts are common (worker's full system_prompt +
  // work item context + ancestor summaries). Default-collapse so the
  // chat stays focused on what the model did, not the instructions.
  const long = prompt.length > 600;
  return (
    <div className="rounded-lg border border-amber-300/40 bg-amber-50/40 p-3 dark:bg-amber-950/20">
      <details open={!long}>
        <summary className="cursor-pointer select-none text-xs font-medium uppercase tracking-wide text-amber-700 dark:text-amber-400">
          System prompt {long ? "(click to expand)" : ""}
        </summary>
        <div className="mt-2 max-h-72 overflow-auto">
          <Markdown>{prompt}</Markdown>
        </div>
      </details>
    </div>
  );
}

function SystemNote({ text, ts }: { text: string; ts: Date }) {
  return (
    <div className="flex items-center gap-2 px-1 text-xs text-muted-foreground">
      <span className="inline-block h-1 w-1 rounded-full bg-muted-foreground/50" />
      <span className="font-medium">{text}</span>
      <span className="text-[10px] opacity-60">{ts.toLocaleTimeString()}</span>
    </div>
  );
}

function AssistantBubble({ chunks }: { chunks: ParsedTextChunk[] }) {
  // Concatenate consecutive text chunks — opencode emits one
  // `text` event per streaming token chunk; the user sees one
  // assistant message, not N bubbles per token.
  const text = chunks.map((c) => c.text).join("");
  const lastTs = chunks[chunks.length - 1].occurredAt;
  return (
    <div className="flex justify-start">
      <div className="max-w-[85%] rounded-lg rounded-tl-sm border border-border bg-card px-3 py-2 text-sm leading-relaxed shadow-sm">
        <div className="mb-1 flex items-center gap-2 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
          <span>assistant</span>
          <span className="opacity-60">{lastTs.toLocaleTimeString()}</span>
        </div>
        <Markdown>{text}</Markdown>
      </div>
    </div>
  );
}

function ReasoningBubble({ chunks }: { chunks: ParsedReasoningChunk[] }) {
  // Reasoning is the model's "thinking" content (only emitted when
  // opencode is started with --thinking). Render it in a distinct
  // style — italic, dimmed, slightly inset — so it reads as
  // meta-content alongside the actual answer. Collapsed by default
  // if it gets long so it doesn't dominate the chat; expanded on
  // click.
  const text = chunks.map((c) => c.text).join("");
  const lastTs = chunks[chunks.length - 1].occurredAt;
  const long = text.length > 600;
  return (
    <div className="flex justify-start">
      <div className="max-w-[85%] rounded-lg border border-violet-300/30 bg-violet-50/20 px-3 py-2 text-xs italic leading-relaxed text-muted-foreground dark:bg-violet-950/10">
      <details open={true}>
          <summary className="cursor-pointer select-none text-[10px] font-medium not-italic uppercase tracking-wide text-violet-700 dark:text-violet-300">
            <span className="inline-flex items-center gap-1">
              <span className="inline-block h-1.5 w-1.5 rounded-full bg-violet-500" />
              reasoning
            </span>
            <span className="ml-2 opacity-60 not-italic">{lastTs.toLocaleTimeString()}</span>
            {long && (
              <span className="ml-2 text-[10px] opacity-60 not-italic">
                (click to expand)
              </span>
            )}
          </summary>
          <div className="mt-2 text-xs not-italic">
            <Markdown>{text}</Markdown>
          </div>
        </details>
      </div>
    </div>
  );
}

function ToolCard({ tool }: { tool: ParsedToolCall }) {
  // Detect subagent spawns: opencode invokes the `task` tool to start
  // a child session. We surface that explicitly so the operator
  // notices the parent is delegating rather than running tools
  // directly. (The actual child session appears in the events of its
  // own WorkerExecution — we can't show its chat here, but we can
  // flag the spawn so it's not buried in a generic "tool" card.)
  const isSubagent = tool.toolName === "task" || tool.toolName === "subagent";
  const accent = isSubagent
    ? "border-violet-300/60 bg-violet-50/30 dark:bg-violet-950/20"
    : tool.output
      ? "border-emerald-300/40 bg-emerald-50/20 dark:bg-emerald-950/15"
      : "border-amber-300/40 bg-amber-50/20 dark:bg-amber-950/15";

  return (
    <div className={cn("rounded-lg border p-3", accent)}>
      <div className="mb-1 flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span
            className={cn(
              "inline-flex items-center rounded px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wide",
              isSubagent
                ? "bg-violet-200 text-violet-800 dark:bg-violet-900 dark:text-violet-200"
                : "bg-amber-200 text-amber-800 dark:bg-amber-900 dark:text-amber-200",
            )}
          >
            {isSubagent ? "subagent" : "tool"}
          </span>
          <span className="font-mono text-xs font-medium">{tool.toolName}</span>
        </div>
        <span className="text-[10px] text-muted-foreground">
          {tool.occurredAt.toLocaleTimeString()}
        </span>
      </div>
      {tool.input && (
        <div className="mt-1">
          <div className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
            Input
          </div>
          <pre className="mt-0.5 max-h-40 overflow-auto rounded bg-background/70 p-2 text-xs">
            {truncate(tool.input, 2000)}
          </pre>
        </div>
      )}
      {tool.output && (
        <div className="mt-1.5">
          <div className="text-[10px] font-medium uppercase tracking-wide text-emerald-700 dark:text-emerald-400">
            Result
          </div>
          <pre className="mt-0.5 max-h-40 overflow-auto rounded bg-background/70 p-2 text-xs">
            {truncate(tool.output, 2000)}
          </pre>
        </div>
      )}
      {!tool.output && (
        <div className="mt-1 text-[11px] italic text-muted-foreground">
          awaiting result…
        </div>
      )}
    </div>
  );
}

function ResultCard({ result }: { result: ParsedResult }) {
  return (
    <div className="flex justify-end">
      <div className="max-w-[85%] rounded-lg rounded-tr-sm border border-blue-300/40 bg-blue-50/30 px-3 py-2 text-sm leading-relaxed shadow-sm dark:bg-blue-950/20">
        <div className="mb-1 flex items-center gap-2 text-[10px] font-medium uppercase tracking-wide text-blue-700 dark:text-blue-300">
          <span>final result</span>
          <span className="opacity-60">{result.occurredAt.toLocaleTimeString()}</span>
        </div>
        <Markdown>{result.text}</Markdown>
      </div>
    </div>
  );
}

function ErrorCard({ error }: { error: ParsedError }) {
  return (
    <div className="rounded-lg border border-destructive/50 bg-destructive/10 p-3">
      <div className="mb-1 flex items-center gap-2">
        <span className="inline-flex items-center rounded bg-destructive/30 px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wide text-destructive">
          error
        </span>
        <span className="text-[10px] text-muted-foreground">
          {error.occurredAt.toLocaleTimeString()}
        </span>
      </div>
      <Markdown>{error.message}</Markdown>
    </div>
  );
}

function ArtifactCard({ artifact }: { artifact: ParsedArtifact }) {
  const fileName = artifact.name.split("/").pop() || artifact.name;
  const isMarkdown = artifact.artifactType === "markdown" || fileName.endsWith(".md");

  return (
    <div className="rounded-lg border border-sky-300/40 bg-sky-50/20 p-3 dark:bg-sky-950/15">
      <div className="mb-2 flex items-center justify-between gap-2">
        <div className="flex items-center gap-2 min-w-0">
          <span className="inline-flex items-center rounded bg-sky-200 px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wide text-sky-800 dark:bg-sky-900 dark:text-sky-200">
            artifact
          </span>
          <span className="truncate font-mono text-xs font-medium">{fileName}</span>
          <span className="shrink-0 text-[10px] text-muted-foreground">
            {artifact.content.length.toLocaleString()} bytes
          </span>
        </div>
        <span className="shrink-0 text-[10px] text-muted-foreground">
          {artifact.occurredAt.toLocaleTimeString()}
        </span>
      </div>
      <details open={true}>
        <summary className="cursor-pointer select-none text-[10px] font-medium uppercase tracking-wide text-sky-700 dark:text-sky-300">
          {isMarkdown ? "Preview" : "Content"}
          {artifact.content.length > 10000 && <span className="ml-2 opacity-60">(long — {artifact.content.length.toLocaleString()} chars)</span>}
        </summary>
        {isMarkdown ? (
          <div className="mt-2 max-h-96 overflow-auto rounded bg-background/70 p-3 leading-relaxed">
            <Markdown>{artifact.content}</Markdown>
          </div>
        ) : (
          <pre className="mt-2 max-h-96 overflow-auto whitespace-pre-wrap break-words rounded bg-background/70 p-3 font-mono text-xs leading-relaxed">
            {artifact.content}
          </pre>
        )}
      </details>
    </div>
  );
}

function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return s.slice(0, max) + "\n…(truncated)";
}