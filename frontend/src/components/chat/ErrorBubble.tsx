interface ErrorBubbleProps {
  text: string;
  className?: string;
}

export function ErrorBubble({ text, className }: ErrorBubbleProps) {
  return (
    <div className={`flex justify-start pl-2 ${className ?? ""}`}>
      <div className="max-w-[92%] rounded-xl border border-destructive/50 bg-destructive/10 px-3 py-2 text-xs text-destructive">
        {text}
      </div>
    </div>
  );
}
