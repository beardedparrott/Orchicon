import { Markdown } from "@/components/markdown";

interface UserBubbleProps {
  text: string;
  source?: string;
  className?: string;
}

export function UserBubble({ text, source = "you", className }: UserBubbleProps) {
  const label =
    source === "nudge"
      ? "liveness check"
      : source === "human" || source === "follow_up"
        ? "you"
        : "goal";

  return (
    <div className={`flex justify-end ${className ?? ""}`}>
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
