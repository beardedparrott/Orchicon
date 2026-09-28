// NoticeBubble — the platform's own record about a conversation.
//
// A message with role "system" is not something the model said: it is the SERVER
// speaking about the conversation. Today that means one thing — the context was
// compacted because it filled the model's window, which on the conversation that
// motivated it read "compacted 2343 messages into 1 summary + 7 recent messages".
//
// It gets its own muted, labelled shell rather than the assistant's, because the
// distinction the operator has to make when they see that line is exactly "who said
// this?" — and the answer is not the model. Paragraphs are preserved rather than
// re-flowed: the notices are written as (what was lost) / (why it ran) / (where the
// original went), and running them together would bury the archive path.

interface NoticeBubbleProps {
  text: string;
  className?: string;
}

export function NoticeBubble({ text, className }: NoticeBubbleProps) {
  const paragraphs = text
    .split(/\n{2,}/)
    .map((p) => p.trim())
    .filter(Boolean);

  return (
    <div className={`flex justify-start pl-2 ${className ?? ""}`} data-testid="notice-bubble">
      <div className="max-w-[92%] rounded-xl border border-border/60 bg-muted/40 px-3 py-2">
        <p className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
          notice
        </p>
        {paragraphs.map((para, i) => (
          <p
            key={i}
            className="mt-1 break-words text-xs leading-relaxed text-muted-foreground [overflow-wrap:anywhere]"
          >
            {para}
          </p>
        ))}
      </div>
    </div>
  );
}
