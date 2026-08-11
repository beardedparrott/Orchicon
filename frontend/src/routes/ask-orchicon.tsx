import { createRoute } from "@tanstack/react-router";
import { useState, useCallback, useRef, useEffect, useMemo } from "react";
import {
  Plus,
  Trash2,
  Paperclip,
  Mic,
  Square,
  RefreshCw,
} from "lucide-react";

import { Route as rootRoute } from "@/routes/__root";

import { useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  useListConversations,
  useCreateConversation,
  useDeleteConversation,
  useListMessages,
  useGetConversation,
  useAbortConversationTurn,
  askKeys,
} from "@/api/askOrchicon";
import { askOrchiconClient } from "@/api/clients";
import { useToast, useToastStore } from "@/components/ui/toast";
import type { ChatMessage } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import { AttachmentInput } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import {
  UserBubble,
  AssistantBubble,
  ErrorBubble,
  ReasoningBubble,
  ChatScrollContainer,
} from "@/components/chat";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/ask-orchicon",
  component: AskOrchiconPage,
});

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

function AskOrchiconPage() {
  const [activeConvId, setActiveConvId] = useState<string | null>(null);
  const [isStreaming, setIsStreaming] = useState(false);
  const [isThinking, setIsThinking] = useState(false);
  const [optimisticUserMsg, setOptimisticUserMsg] = useState<string | null>(
    null,
  );
  const [pendingReplyId, setPendingReplyId] = useState<string | null>(null);
  const toast = useToast();

  // Live streaming items accumulated from ChatStream events.
  const [streamItems, setStreamItems] = useState<StreamItem[]>([]);
  const streamPhaseRef = useRef(0);
  const prevConvIdRef = useRef<string | null>(null);

  const { data: conversations, isLoading: convsLoading } =
    useListConversations();
  const { data: messages, isLoading: msgsLoading } = useListMessages(
    activeConvId ?? "",
    { refetchInterval: isStreaming ? 2000 : false },
  );
  const { data: activeConv } = useGetConversation(activeConvId ?? "");
  const createConv = useCreateConversation();
  const deleteConv = useDeleteConversation();
  const abortTurn = useAbortConversationTurn();
  const qc = useQueryClient();

  // Switching conversations resets local state.
  // Skip the reset on null→id transitions (greeting path) so the streaming
  // state set by sendStreaming is not clobbered by this effect.
  useEffect(() => {
    const prev = prevConvIdRef.current;
    prevConvIdRef.current = activeConvId;
    if (prev === null || activeConvId === null) return;
    // Switching between two non-null conversation ids — reset.
    setPendingReplyId(null);
    setIsStreaming(false);
    setIsThinking(false);
    setOptimisticUserMsg(null);
    setStreamItems([]);
    streamPhaseRef.current = 0;
  }, [activeConvId]);

  // The reply (or error) is persisted under the acked assistant message id.
  // When it appears via polling, the turn is over — clear the pending state.
  useEffect(() => {
    if (!isStreaming || !pendingReplyId || !messages) return;
    if (messages.some((m) => m.id === pendingReplyId)) {
      setPendingReplyId(null);
      setIsStreaming(false);
      setIsThinking(false);
      setOptimisticUserMsg(null);
      setStreamItems([]);
      streamPhaseRef.current = 0;
      qc.invalidateQueries({ queryKey: askKeys.conversations });
    }
  }, [messages, isStreaming, pendingReplyId, qc]);

  const handleNewChat = useCallback(async () => {
    try {
      const conv = await createConv.mutateAsync({});
      if (conv?.id) {
        setActiveConvId(conv.id);
      }
    } catch {
      toast.error("Failed to create conversation", { title: "Error" });
    }
  }, [createConv, toast]);

  const handleDeleteConv = useCallback(
    async (id: string, e: React.MouseEvent) => {
      e.stopPropagation();
      try {
        await deleteConv.mutateAsync(id);
        if (activeConvId === id) {
          setActiveConvId(null);
        }
      } catch {
        toast.error("Failed to delete conversation", { title: "Error" });
      }
    },
    [deleteConv, activeConvId, toast],
  );

  const handleStopStreaming = useCallback(async () => {
    if (!activeConvId) return;
    try {
      await abortTurn.mutateAsync(activeConvId);
    } catch {
      toast.error("Failed to stop the reply", { title: "Error" });
    }
  }, [activeConvId, abortTurn, toast]);

  // Streaming helper — takes convId as a parameter so it is never stale.
  const sendStreaming = useCallback(
    async (convId: string, text: string, attachments?: AttachmentInput[]) => {
      setOptimisticUserMsg(text);
      setIsStreaming(true);
      setIsThinking(true);
      setPendingReplyId(null);
      setStreamItems([]);
      streamPhaseRef.current = 0;

      try {
        const stream = askOrchiconClient.chatStream({
          conversationId: convId,
          message: text,
          attachments: attachments ?? [],
        });
        let acked = false;
        for await (const chunk of stream) {
          if (chunk.event.case === "turnStarted") {
            setPendingReplyId(chunk.event.value.assistantMessageId);
            acked = true;
          } else if (chunk.event.case === "textChunk") {
            const content = chunk.event.value.content;
            if (content) {
              const phase = `p-${streamPhaseRef.current}`;
              setStreamItems((prev) => [
                ...prev,
                {
                  kind: "text",
                  text: content,
                  at: Date.now(),
                  key: `st-${Date.now()}-${Math.random()}`,
                  phase,
                },
              ]);
            }
          } else if (chunk.event.case === "reasoning") {
            const content = chunk.event.value.content;
            if (content) {
              const phase = `p-${streamPhaseRef.current}`;
              setStreamItems((prev) => [
                ...prev,
                {
                  kind: "reasoning",
                  text: content,
                  at: Date.now(),
                  key: `sr-${Date.now()}-${Math.random()}`,
                  phase,
                },
              ]);
            }
          } else if (chunk.event.case === "error") {
            toast.error(chunk.event.value.message);
            setIsStreaming(false);
            setIsThinking(false);
            setOptimisticUserMsg(null);
            setStreamItems([]);
          }
        }
        if (!acked) {
          setIsStreaming(false);
          setIsThinking(false);
          setOptimisticUserMsg(null);
          setStreamItems([]);
        }
      } catch (err: unknown) {
        setIsStreaming(false);
        setIsThinking(false);
        setOptimisticUserMsg(null);
        setStreamItems([]);
        toast.error(String(err instanceof Error ? err.message : err), { title: "Chat error" });
      }
    },
    [toast],
  );

  const handleSendMessage = useCallback(
    async (text: string, attachments?: AttachmentInput[]) => {
      if (!text.trim() || !activeConvId || isStreaming) return;
      await sendStreaming(activeConvId, text, attachments);
    },
    [activeConvId, isStreaming, sendStreaming],
  );

  const handleRetry = useCallback(() => {
    const lastUser = messages
      ?.slice()
      .reverse()
      .find((m) => m.role === "user");
    if (lastUser?.content) {
      handleSendMessage(lastUser.content);
    }
  }, [messages, handleSendMessage]);

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
    <div className="-m-6 lg:-m-8 flex h-[calc(100vh-3.5rem)] gap-0">
      {/* Main chat area — centered column */}
      <div className="flex flex-1 flex-col min-w-0">
        {!activeConvId ? (
          /* --- Greeting state: centered vertical + horizontal --- */
          <div className="flex flex-1 items-center justify-center px-4">
            <div className="w-full max-w-2xl space-y-6">
              <div className="text-center space-y-3">
                <h1 className="text-2xl font-semibold text-foreground">
                  What would you like to create today?
                </h1>
                <p className="text-sm text-muted-foreground">
                  Ask Orchicon anything — create projects, manage work items,
                  brainstorm ideas, or get help with your codebase.
                </p>
              </div>
              <ChatInputField
                onSend={async (text, attachments) => {
                  // Create a conversation first, then send via the streaming
                  // helper directly (avoids the stale-closure on handleSendMessage).
                  try {
                    const conv = await createConv.mutateAsync({});
                    if (conv?.id) {
                      setActiveConvId(conv.id);
                      await sendStreaming(conv.id, text, attachments);
                    }
                  } catch {
                    toast.error("Failed to create conversation", {
                      title: "Error",
                    });
                  }
                }}
                onStop={handleStopStreaming}
                isStreaming={isStreaming}
                placeholder="Ask Orchicon anything..."
              />
            </div>
          </div>
        ) : (
          /* --- Active conversation: scroll + pinned input --- */
          <div className="flex flex-1 flex-col min-h-0">
            {/* Chat header */}
            <div className="flex items-center justify-between border-b px-6 py-3 shrink-0">
              <h2 className="text-sm font-medium truncate">
                {activeConv?.title || "Ask Orchicon"}
              </h2>
              <Button variant="ghost" size="sm" onClick={handleNewChat}>
                <Plus className="h-4 w-4" />
              </Button>
            </div>

            {/* Messages — auto-stick scroll */}
            <ChatScrollContainer
              items={[displayMessages, isThinking, optimisticUserMsg]}
            >
              <div className="mx-auto max-w-3xl space-y-4 px-4 py-6">
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
                          <ReasoningBubble key={item.key} text={item.text} />
                        );
                      case "error":
                        return (
                          <ErrorBubble key={item.key} text={item.text} />
                        );
                      default:
                        return null;
                    }
                  })}

                {/* Thinking indicator — visible until the first text chunk arrives */}
                {isThinking && !groupedStream.some((i) => i.kind === "text") && (
                  <div className="flex justify-start">
                    <div className="rounded-2xl rounded-tl-sm border border-sky-300/30 bg-sky-50/20 px-4 py-3 dark:border-sky-950/40 dark:bg-sky-950/10">
                      <div className="flex items-center gap-2">
                        <span className="inline-block h-1.5 w-1.5 rounded-full bg-sky-500 animate-pulse" />
                        <span className="text-sm text-muted-foreground">
                          Orchicon is thinking…
                        </span>
                      </div>
                    </div>
                  </div>
                )}

                <div className="h-4" />
              </div>
            </ChatScrollContainer>

            {/* Input — fixed at bottom */}
            <div className="shrink-0 border-t">
              <div className="mx-auto max-w-3xl">
                <ChatInputField
                  onSend={handleSendMessage}
                  onStop={handleStopStreaming}
                  isStreaming={isStreaming}
                  placeholder="Ask Orchicon anything..."
                />
              </div>
            </div>
          </div>
        )}
      </div>

      {/* Right sidebar — conversations panel (w-80 per ADR-001) */}
      <aside className="hidden lg:flex w-80 shrink-0 flex-col border-l bg-card overflow-y-auto">
        <div className="flex items-center justify-between border-b px-4 py-3">
          <span className="text-sm font-medium">Conversations</span>
          <Button
            variant="ghost"
            size="sm"
            onClick={handleNewChat}
            disabled={createConv.isPending}
          >
            <Plus className="h-4 w-4" />
          </Button>
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
          {conversations?.map((conv) => (
            <button
              key={conv.id}
              onClick={() => setActiveConvId(conv.id)}
              className={cn(
                "w-full text-left rounded-md px-3 py-2 text-sm transition-colors group",
                activeConvId === conv.id
                  ? "bg-accent text-accent-foreground"
                  : "text-muted-foreground hover:bg-accent/50 hover:text-accent-foreground",
              )}
            >
              <div className="flex items-start justify-between gap-2">
                <span className="truncate flex-1">
                  {conv.title || "New conversation"}
                </span>
                <button
                  onClick={(e) => handleDeleteConv(conv.id, e)}
                  className="shrink-0 opacity-0 group-hover:opacity-100 text-muted-foreground hover:text-destructive"
                >
                  <Trash2 className="h-3 w-3" />
                </button>
              </div>
              {conv.lastMessagePreview && (
                <p className="mt-0.5 text-xs text-muted-foreground truncate">
                  {conv.lastMessagePreview}
                </p>
              )}
            </button>
          ))}
        </div>
      </aside>
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
    return (
      <div className="flex justify-start">
        <div className="max-w-[88%] rounded-2xl rounded-tl-sm border border-destructive/50 bg-destructive/10 px-4 py-3">
          <p className="text-sm text-destructive mb-1">
            {message.metadata?.error}
          </p>
          {onRetry && (
            <Button variant="outline" size="sm" onClick={onRetry}>
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
}: {
  onSend: (text: string, attachments?: AttachmentInput[]) => void;
  onStop: () => void;
  isStreaming: boolean;
  placeholder?: string;
}) {
  const disabled = isStreaming;
  const [text, setText] = useState("");
  const [attachments, setAttachments] = useState<AttachmentInput[]>([]);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const handleSubmit = useCallback(() => {
    if ((text.trim() || attachments.length > 0) && !disabled) {
      onSend(
        text.trim(),
        attachments.length > 0 ? attachments : undefined,
      );
      setText("");
      setAttachments([]);
      if (inputRef.current) {
        inputRef.current.style.height = "auto";
      }
    }
  }, [text, attachments, disabled, onSend]);

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
      <div className="flex gap-2 items-end">
        <div className="flex-1 flex items-end gap-1 rounded-xl border bg-background px-3 py-2 focus-within:ring-2 focus-within:ring-ring focus-within:ring-offset-2">
          <button
            onClick={() => fileInputRef.current?.click()}
            disabled={disabled}
            className="shrink-0 rounded p-1 text-muted-foreground hover:text-foreground hover:bg-accent disabled:opacity-50"
            title="Attach file"
          >
            <Paperclip className="h-4 w-4" />
          </button>
          <textarea
            ref={inputRef}
            value={text}
            onChange={(e) => {
              setText(e.target.value);
              e.target.style.height = "auto";
              e.target.style.height = `${Math.min(e.target.scrollHeight, 192)}px`;
            }}
            onKeyDown={handleKeyDown}
            placeholder={placeholder}
            disabled={disabled}
            rows={1}
            className="flex-1 resize-none bg-transparent px-1 py-0 text-sm leading-6 outline-none placeholder:text-muted-foreground disabled:opacity-50 min-h-[24px] max-h-[192px]"
          />
          <button
            onClick={isRecording ? handleStopRecording : handleVoiceInput}
            disabled={disabled}
            className={cn(
              "shrink-0 rounded p-1 disabled:opacity-50",
              isRecording
                ? "text-destructive hover:bg-destructive/10 animate-pulse"
                : "text-muted-foreground hover:text-foreground hover:bg-accent",
            )}
            title={isRecording ? "Stop recording" : "Voice input"}
          >
            <Mic className="h-4 w-4" />
          </button>
        </div>
        {isStreaming ? (
          <Button onClick={onStop} variant="destructive" size="sm">
            <Square className="h-4 w-4 mr-1" />
            Stop
          </Button>
        ) : (
          <Button
            onClick={handleSubmit}
            disabled={(!text.trim() && attachments.length === 0) || disabled}
            size="sm"
          >
            Send
          </Button>
        )}
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

export default AskOrchiconPage;
