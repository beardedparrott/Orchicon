import { useCallback, useRef, useState } from "react";
import { Brain, Settings2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { ConversationMode } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";

interface ModeToggleProps {
  mode: ConversationMode;
  onModeChange: (mode: ConversationMode) => void;
  disabled?: boolean;
  className?: string;
}

const options = [
  { value: ConversationMode.BRAINSTORM, label: "Brainstorm", icon: Brain },
  { value: ConversationMode.ORCHICON, label: "Orchicon", icon: Settings2 },
] as const;

export function ModeToggle({
  mode,
  onModeChange,
  disabled,
  className,
}: ModeToggleProps) {
  const [announcement, setAnnouncement] = useState("");
  const groupRef = useRef<HTMLDivElement>(null);

  const handleChange = useCallback(
    (next: ConversationMode) => {
      if (next === mode || disabled) return;
      onModeChange(next);
      const label = next === ConversationMode.BRAINSTORM ? "Brainstorm" : "Orchicon";
      setAnnouncement(`Switched to ${label} mode`);
    },
    [mode, disabled, onModeChange],
  );

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      const currentIndex = options.findIndex((o) => o.value === mode);
      let nextIndex = currentIndex;

      switch (e.key) {
        case "ArrowRight":
        case "ArrowDown":
          e.preventDefault();
          nextIndex = (currentIndex + 1) % options.length;
          break;
        case "ArrowLeft":
        case "ArrowUp":
          e.preventDefault();
          nextIndex = (currentIndex - 1 + options.length) % options.length;
          break;
        case "Home":
          e.preventDefault();
          nextIndex = 0;
          break;
        case "End":
          e.preventDefault();
          nextIndex = options.length - 1;
          break;
        case "Enter":
        case " ":
          e.preventDefault();
          handleChange(options[currentIndex].value);
          return;
        default:
          return;
      }

      const next = options[nextIndex];
      handleChange(next.value);
      // Move focus to the active button
      const buttons = groupRef.current?.querySelectorAll('[role="radio"]');
      buttons?.[nextIndex]?.focus();
    },
    [mode, handleChange],
  );

  return (
    <>
      <div
        ref={groupRef}
        role="radiogroup"
        aria-label="Conversation mode"
        className={cn(
          "inline-flex items-center rounded-full border bg-muted p-0.5",
          className,
        )}
        onKeyDown={handleKeyDown}
      >
        {options.map((opt) => {
          const active = mode === opt.value;
          const Icon = opt.icon;
          return (
            <button
              key={opt.value}
              role="radio"
              type="button"
              aria-checked={active}
              aria-label={opt.label}
              disabled={disabled}
              title={opt.label}
              onClick={() => handleChange(opt.value)}
              className={cn(
                "inline-flex items-center justify-center rounded-full px-1.5 py-1 sm:px-2 text-xs font-medium transition-colors duration-150",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
                "disabled:opacity-50 disabled:pointer-events-none",
                active
                  ? "bg-primary text-primary-foreground shadow-sm"
                  : "bg-transparent text-muted-foreground hover:text-foreground",
              )}
            >
              <Icon className="h-3.5 w-3.5 sm:hidden" />
              <span className="hidden sm:inline">{opt.label}</span>
            </button>
          );
        })}
      </div>
      <span className="sr-only" role="status" aria-live="polite">
        {announcement}
      </span>
    </>
  );
}
