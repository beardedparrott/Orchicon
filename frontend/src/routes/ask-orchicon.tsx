import { createRoute } from "@tanstack/react-router";
import { useState, useCallback, useRef, useEffect } from "react";
import { MessageSquare, Plus, Trash2, Paperclip, Mic, Square, Copy, Check, RefreshCw } from "lucide-react";

import { Route as rootRoute } from "@/routes/__root";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useListConversations, useCreateConversation, useDeleteConversation, useListMessages, useGetConversation, useAbortConversationTurn } from "@/api/askOrchicon";
import { askOrchiconClient } from "@/api/clients";
import { useToast, useToastStore } from "@/components/ui/toast";
import type { ChatMessage } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import { AttachmentInput } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import { Markdown } from "@/components/markdown";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/ask-orchicon",
  component: AskOrchiconPage,
});

	function AskOrchiconPage() {
  const [activeConvId, setActiveConvId] = useState<string | null>(null);
  const [isStreaming, setIsStreaming] = useState(false);
  const [isThinking, setIsThinking] = useState(false);
  const [optimisticUserMsg, setOptimisticUserMsg] = useState<string | null>(null);
  // pendingReplyId is the assistant message id from the TurnStarted ack. It
  // stays set (and the messages query keeps polling) until the detached
  // collector persists the reply — or an error — under that id.
  const [pendingReplyId, setPendingReplyId] = useState<string | null>(null);
  const toast = useToast();

  const { data: conversations, isLoading: convsLoading } = useListConversations();
  const { data: messages, isLoading: msgsLoading } = useListMessages(activeConvId ?? "", {
    refetchInterval: isStreaming ? 2000 : false,
  });
  const { data: activeConv } = useGetConversation(activeConvId ?? "");
  const createConv = useCreateConversation();
  const deleteConv = useDeleteConversation();
  const abortTurn = useAbortConversationTurn();

  const chatEndRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    chatEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages, isThinking, optimisticUserMsg]);

  // Switching conversations resets the local pending state.
  useEffect(() => {
    setPendingReplyId(null);
    setIsStreaming(false);
    setIsThinking(false);
    setOptimisticUserMsg(null);
  }, [activeConvId]);

  // The reply (or error) is persisted under the acked assistant message id.
  // When it appears via polling, the turn is over — clear the pending state
  // so polling stops and the input re-enables.
  useEffect(() => {
    if (!isStreaming || !pendingReplyId || !messages) return;
    if (messages.some(m => m.id === pendingReplyId)) {
      setPendingReplyId(null);
      setIsStreaming(false);
      setIsThinking(false);
      setOptimisticUserMsg(null);
    }
  }, [messages, isStreaming, pendingReplyId]);

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

  const handleDeleteConv = useCallback(async (id: string, e: React.MouseEvent) => {
    e.stopPropagation();
    try {
      await deleteConv.mutateAsync(id);
      if (activeConvId === id) {
        setActiveConvId(null);
      }
    } catch {
      toast.error("Failed to delete conversation", { title: "Error" });
    }
  }, [deleteConv, activeConvId, toast]);

	// Stop: an explicit server-side abort. The collector cancels promptly and
	// persists a "Turn stopped by the user." error message; the poll picks it
	// up and re-enables the input. The conversation's session stays alive for
	// the next message.
	const handleStopStreaming = useCallback(async () => {
		if (!activeConvId) return;
		try {
			await abortTurn.mutateAsync(activeConvId);
		} catch {
			toast.error("Failed to stop the reply", { title: "Error" });
		}
	}, [activeConvId, abortTurn, toast]);

	const handleSendMessage = useCallback(async (text: string, attachments?: AttachmentInput[]) => {
		if (!text.trim() || !activeConvId || isStreaming) return;

		// Show the user's message immediately; the reply arrives by polling
		// (Send returns right after the ack).
		setOptimisticUserMsg(text);
		setIsStreaming(true);
		setIsThinking(true);
		setPendingReplyId(null);

		try {
			const stream = askOrchiconClient.chatStream(
				{ conversationId: activeConvId, message: text, attachments: attachments ?? [] }
			);
			let acked = false;
			for await (const chunk of stream) {
				if (chunk.event.case === "turnStarted") {
					setPendingReplyId(chunk.event.value.assistantMessageId);
					acked = true;
				} else if (chunk.event.case === "error") {
					toast.error(chunk.event.value.message);
					setIsStreaming(false);
					setIsThinking(false);
					setOptimisticUserMsg(null);
				}
			}
			if (!acked) {
				// No ack received: the turn may still be running server-side,
				// but with no message id to poll for, don't hang the UI.
				setIsStreaming(false);
				setIsThinking(false);
				setOptimisticUserMsg(null);
			}
		} catch (err: any) {
			// Synchronous RPC failure (e.g. a reply already in progress) —
			// the message was not dispatched.
			setIsStreaming(false);
			setIsThinking(false);
			setOptimisticUserMsg(null);
			toast.error(String(err?.message ?? err), { title: "Chat error" });
		}
	}, [activeConvId, isStreaming, toast]);

	// Retry re-sends the most recent user message in the same conversation
	// (the error message it follows was persisted by a failed turn).
	// messages is chronological (oldest-first), so `find` would return the
	// FIRST user message — iterate from the end for the LAST (most recent).
	const handleRetry = useCallback(() => {
		const lastUser = messages?.slice().reverse().find(m => m.role === "user");
		if (lastUser?.content) {
			handleSendMessage(lastUser.content);
		}
	}, [messages, handleSendMessage]);

  return (
    <div className="-m-6 lg:-m-8 flex h-[calc(100vh-3.5rem)] gap-0">
      {/* Main chat area */}
      <div className="flex flex-1 flex-col min-w-0">
        {!activeConvId ? (
          <div className="flex flex-1 items-center justify-center">
            <div className="text-center space-y-4">
              <MessageSquare className="mx-auto h-12 w-12 text-muted-foreground" />
              <h2 className="text-xl font-semibold">Ask Orchicon</h2>
              <p className="text-sm text-muted-foreground max-w-md">
                Your AI assistant for managing Orchicon. I can create projects, manage
                work items, diagnose failures, and answer questions about your data.
              </p>
              <Button onClick={handleNewChat} disabled={createConv.isPending}>
                <Plus className="mr-2 h-4 w-4" />
                New Conversation
              </Button>
            </div>
          </div>
        ) : (
          <div className="flex flex-1 flex-col min-h-0">
            {/* Chat header */}
            <div className="flex items-center justify-between border-b px-6 py-3">
              <h2 className="text-sm font-medium truncate">
                {activeConv?.title || "Ask Orchicon"}
              </h2>
              <Button variant="ghost" size="sm" onClick={handleNewChat}>
                <Plus className="h-4 w-4" />
              </Button>
            </div>

            {/* Messages */}
            <div className="flex-1 min-h-0 overflow-y-auto px-4 py-4 space-y-4">
              {msgsLoading && (
                <p className="text-center text-sm text-muted-foreground">Loading messages...</p>
              )}
              {!msgsLoading && messages?.length === 0 && !optimisticUserMsg && (
                <div className="text-center text-sm text-muted-foreground py-8">
                  Start a conversation by typing a message below.
                </div>
              )}

              {/* Persisted messages from the server */}
              {messages?.map((msg) => (
                <ChatBubble key={msg.id} message={msg} onRetry={handleRetry} />
              ))}

              {/* Optimistic user message (shown immediately) */}
              {optimisticUserMsg && !messages?.some(m => m.content === optimisticUserMsg && m.role === "user") && (
                <div className="flex justify-end">
                  <div className="rounded-lg bg-primary text-primary-foreground px-4 py-2.5 max-w-[80%]">
                    <p className="text-sm whitespace-pre-wrap">{optimisticUserMsg}</p>
                  </div>
                </div>
              )}

		{/* Thinking indicator */}
				{isThinking && (
					<div className="flex justify-start">
						<div className="rounded-lg bg-card border px-4 py-2.5 min-w-[280px] max-w-[80%]">
							<p className="text-xs font-medium text-muted-foreground mb-1">Orchicon</p>
							<div className="flex items-center gap-2">
								<div className="flex gap-0.5">
									<span className="h-1.5 w-1.5 rounded-full bg-muted-foreground animate-bounce [animation-delay:-0.3s]" />
									<span className="h-1.5 w-1.5 rounded-full bg-muted-foreground animate-bounce [animation-delay:-0.15s]" />
									<span className="h-1.5 w-1.5 rounded-full bg-muted-foreground animate-bounce" />
								</div>
								<p className="text-sm italic text-muted-foreground">
									Orchicon is thinking about your request…
								</p>
							</div>
						</div>
					</div>
				)}

              <div ref={chatEndRef} />
            </div>

            {/* Input */}
			<ChatInputField
				onSend={handleSendMessage}
				onStop={handleStopStreaming}
				isStreaming={isStreaming}
			/>
          </div>
        )}
      </div>

      {/* Right sidebar — conversation history */}
      <aside className="hidden lg:flex w-72 shrink-0 flex-col border-l bg-card overflow-y-auto">
        <div className="flex items-center justify-between border-b px-4 py-3">
          <span className="text-sm font-medium">Conversations</span>
          <Button variant="ghost" size="sm" onClick={handleNewChat} disabled={createConv.isPending}>
            <Plus className="h-4 w-4" />
          </Button>
        </div>
        <div className="flex-1 overflow-y-auto p-2 space-y-1">
          {convsLoading && (
            <p className="text-xs text-center text-muted-foreground py-4">Loading...</p>
          )}
          {!convsLoading && (!conversations || conversations.length === 0) && (
            <p className="text-xs text-center text-muted-foreground py-4">No conversations yet</p>
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

function CopyButton({ text }: { text: string }) {
	const [copied, setCopied] = useState(false);
	return (
		<button
			onClick={() => {
				navigator.clipboard.writeText(text);
				setCopied(true);
				setTimeout(() => setCopied(false), 2000);
			}}
			className="shrink-0 rounded p-1 bg-muted/60 hover:bg-accent hover:text-foreground transition-colors float-right -mt-0.5 -mr-1"
			title="Copy"
		>
			{copied ? <Check className="h-3 w-3 text-green-500" /> : <Copy className="h-3 w-3 text-muted-foreground" />}
		</button>
	);
}

function ChatBubble({ message, onRetry }: { message: ChatMessage; onRetry?: () => void }) {
	const isUser = message.role === "user";
	const isError = !!message.metadata?.error;
	return (
		<div className={cn("flex", isUser ? "justify-end" : "justify-start")}>
			<div
				className={cn(
					"rounded-lg px-4 py-2.5 min-w-[280px] max-w-[80%] group relative",
					isUser
						? "bg-primary text-primary-foreground"
						: isError
							? "bg-destructive/10 border border-destructive/40"
							: "bg-card border",
				)}
			>
				{!isUser && (
					<p className={cn("text-xs font-medium mb-1", isError ? "text-destructive" : "text-muted-foreground")}>
						Orchicon
					</p>
				)}
				{isError ? (
					<div className="text-sm">
						<p className="text-destructive">{message.metadata?.error}</p>
						{onRetry && (
							<Button
								variant="outline"
								size="sm"
								className="mt-2"
								onClick={onRetry}
							>
								<RefreshCw className="h-3.5 w-3.5 mr-1" />
								Retry
							</Button>
						)}
					</div>
				) : (
					<>
						<CopyButton text={message.content} />
						<div className="text-sm">
							<Markdown>{message.content}</Markdown>
						</div>
					</>
				)}
			</div>
		</div>
	);
}

function ChatInputField({
	onSend,
	onStop,
	isStreaming,
}: {
	onSend: (text: string, attachments?: AttachmentInput[]) => void;
	onStop: () => void;
	isStreaming: boolean;
}) {
	const disabled = isStreaming;
  const [text, setText] = useState("");
  const [attachments, setAttachments] = useState<AttachmentInput[]>([]);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const handleSubmit = useCallback(() => {
    if ((text.trim() || attachments.length > 0) && !disabled) {
      onSend(text.trim(), attachments.length > 0 ? attachments : undefined);
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

  const handleFileSelect = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => {
      const input = new AttachmentInput();
      input.name = file.name;
      input.mimeType = file.type;
      input.data = new Uint8Array(reader.result as ArrayBuffer);
      setAttachments(prev => [...prev, input]);
    };
    reader.readAsArrayBuffer(file);
    e.target.value = "";
  }, []);

  const [isRecording, setIsRecording] = useState(false);
  const mediaRecorderRef = useRef<MediaRecorder | null>(null);
  const audioChunksRef = useRef<Blob[]>([]);

  const handleVoiceInput = useCallback(async () => {
    const pushToast = useToastStore.getState().push;

    // Path 1: Client-side SpeechRecognition (Chrome/Edge).
    if ('webkitSpeechRecognition' in window || 'SpeechRecognition' in window) {
      const SpeechRecognition = (window as any).SpeechRecognition ?? (window as any).webkitSpeechRecognition;
      const recognition = new SpeechRecognition();
      recognition.lang = 'en-US';
      recognition.interimResults = false;
      recognition.onresult = (event: any) => {
        const transcript = event.results[0][0].transcript;
        setText(prev => (prev ? prev + ' ' : '') + transcript);
      };
      recognition.onerror = () => {
        pushToast({ kind: "error", message: "Voice input failed. Check your microphone permissions." });
      };
      recognition.start();
      return;
    }

    // Path 2: MediaRecorder (Firefox, others). Record audio, attach to chat.
    if (!navigator.mediaDevices?.getUserMedia) {
      pushToast({ kind: "info", message: "Voice input requires a browser with SpeechRecognition (Chrome/Edge) or media recording support." });
      return;
    }

    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      const recorder = new MediaRecorder(stream);
      mediaRecorderRef.current = recorder;
      audioChunksRef.current = [];
      setIsRecording(true);

      recorder.ondataavailable = (e) => {
        if (e.data.size > 0) audioChunksRef.current.push(e.data);
      };

      recorder.onstop = () => {
        setIsRecording(false);
        stream.getTracks().forEach(t => t.stop());
        const blob = new Blob(audioChunksRef.current, { type: "audio/webm" });
        const reader = new FileReader();
        reader.onload = () => {
          const input = new AttachmentInput();
          input.name = "voice_input.webm";
          input.mimeType = "audio/webm";
          input.data = new Uint8Array(reader.result as ArrayBuffer);
          setAttachments(prev => [...prev, input]);
          pushToast({ kind: "success", message: "Audio recorded and attached. Send your message to have Orchicon process it." });
        };
        reader.readAsArrayBuffer(blob);
      };

      recorder.onerror = () => {
        setIsRecording(false);
        stream.getTracks().forEach(t => t.stop());
        pushToast({ kind: "error", message: "Recording failed." });
      };

      recorder.start();
      // Stop after 10 seconds of silence or when mic button is pressed again.
      setTimeout(() => {
        if (recorder.state === "recording") recorder.stop();
      }, 10000);
    } catch {
      pushToast({ kind: "error", message: "Microphone access denied or unavailable." });
    }
  }, []);

  const handleStopRecording = useCallback(() => {
    if (mediaRecorderRef.current?.state === "recording") {
      mediaRecorderRef.current.stop();
    }
  }, []);

  return (
    <div className="border-t p-4">
      {attachments.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-2">
          {attachments.map((a, i) => (
            <span key={i} className="inline-flex items-center gap-1 rounded-md bg-muted px-2 py-1 text-xs">
              <Paperclip className="h-3 w-3" />
              {a.name}
              <button
                onClick={() => setAttachments(prev => prev.filter((_, j) => j !== i))}
                className="ml-1 text-muted-foreground hover:text-foreground"
              >
                ×
              </button>
            </span>
          ))}
        </div>
      )}
      <div className="flex gap-2 items-end">
        <div className="flex-1 flex items-end gap-1 rounded-md border bg-background px-3 py-2 focus-within:ring-2 focus-within:ring-ring">
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
              e.target.style.height = `${Math.min(e.target.scrollHeight, 200)}px`;
            }}
            onKeyDown={handleKeyDown}
            placeholder="Ask Orchicon anything..."
            disabled={disabled}
            rows={1}
            className="flex-1 resize-none bg-transparent px-1 py-0 text-sm outline-none disabled:opacity-50"
          />
          <button
            onClick={isRecording ? handleStopRecording : handleVoiceInput}
            disabled={disabled}
            className={cn(
              "shrink-0 rounded p-1 disabled:opacity-50",
              isRecording
                ? "text-destructive hover:bg-destructive/10 animate-pulse"
                : "text-muted-foreground hover:text-foreground hover:bg-accent"
            )}
            title={isRecording ? "Stop recording" : "Voice input"}
          >
            <Mic className="h-4 w-4" />
          </button>
        </div>
		{isStreaming ? (
			<Button
				onClick={onStop}
				variant="destructive"
				size="sm"
			>
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
