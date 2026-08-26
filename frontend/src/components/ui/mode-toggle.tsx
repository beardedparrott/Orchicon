import { useCallback, useState, useRef, useEffect } from "react";
import { createPortal } from "react-dom";
import { Brain, ChevronDown } from "lucide-react";
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
] as const;

export function ModeToggle({
  mode,
  onModeChange,
  disabled,
  className,
}: ModeToggleProps) {
  const [open, setOpen] = useState(false);
  const [announcement, setAnnouncement] = useState("");
  const ref = useRef<HTMLDivElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const [menuStyle, setMenuStyle] = useState<React.CSSProperties>({});

  // Position the portaled menu above the trigger button.
  useEffect(() => {
    if (open && ref.current) {
      const rect = ref.current.getBoundingClientRect();
      setMenuStyle({
        position: "fixed",
        bottom: `${window.innerHeight - rect.top + 4}px`,
        right: `${window.innerWidth - rect.right}px`,
        minWidth: `${rect.width}px`,
      });
    }
  }, [open]);

  const current = options.find((o) => o.value === mode) ?? options[0];
  const CurrentIcon = current.icon;

  const handleChange = useCallback(
    (next: ConversationMode) => {
      if (next === mode || disabled) return;
      onModeChange(next);
      const label = "Brainstorm";
      setAnnouncement(`Switched to ${label} mode`);
      setOpen(false);
    },
    [mode, disabled, onModeChange],
  );

  // Close on outside click.
  useEffect(() => {
    if (!open) return;
    const handler = (e: MouseEvent) => {
      const target = e.target as Node;
      if (
        (ref.current && ref.current.contains(target)) ||
        (menuRef.current && menuRef.current.contains(target))
      ) {
        return;
      }
      setOpen(false);
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [open]);

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (disabled) return;
      switch (e.key) {
        case "Escape":
          setOpen(false);
          break;
        case "ArrowDown":
        case "ArrowUp": {
          e.preventDefault();
          if (!open) {
            setOpen(true);
          } else {
            const idx = options.findIndex((o) => o.value === mode);
            const next =
              e.key === "ArrowDown"
                ? options[(idx + 1) % options.length]
                : options[(idx - 1 + options.length) % options.length];
            handleChange(next.value);
          }
          break;
        }
        case "Enter":
        case " ":
          e.preventDefault();
          setOpen((v) => !v);
          break;
      }
    },
    [disabled, open, mode, handleChange],
  );

  return (
    <div ref={ref} className={cn("relative", className)}>
      <button
        type="button"
        role="combobox"
        aria-expanded={open}
        aria-label="Conversation mode"
        disabled={disabled}
        onClick={() => setOpen((v) => !v)}
        onKeyDown={handleKeyDown}
        className={cn(
          "inline-flex items-center gap-1 rounded-md glass-input px-2 py-1.5 text-xs font-medium transition-colors",
          "hover:bg-accent hover:text-accent-foreground",
          "focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring",
          "disabled:opacity-50 disabled:pointer-events-none",
        )}
      >
        <CurrentIcon className="h-3.5 w-3.5 text-muted-foreground" />
        <span className="hidden sm:inline">{current.label}</span>
        <ChevronDown
          className={cn(
            "h-3 w-3 text-muted-foreground transition-transform",
            open && "rotate-180",
          )}
        />
      </button>
      {open &&
        createPortal(
          <div
            ref={menuRef}
            role="listbox"
            aria-label="Select mode"
            style={menuStyle}
            className="z-50 min-w-[140px] overflow-hidden rounded-xl glass-menu text-popover-foreground animate-in fade-in-0 zoom-in-95"
          >
            {options.map((opt) => {
              const active = mode === opt.value;
              const Icon = opt.icon;
              return (
                <button
                  key={opt.value}
                  role="option"
                  aria-selected={active}
                  type="button"
                  onClick={() => handleChange(opt.value)}
                  className={cn(
                    "flex w-full items-center gap-2 px-3 py-2 text-xs font-medium transition-colors",
                    "hover:bg-accent hover:text-accent-foreground",
                    "focus:bg-accent focus:text-accent-foreground focus:outline-none",
                    active && "bg-accent text-accent-foreground",
                  )}
                >
                  <Icon className="h-3.5 w-3.5" />
                  {opt.label}
                  {active && (
                    <span className="ml-auto text-primary">✓</span>
                  )}
                </button>
              );
            })}
          </div>,
          document.body,
        )}
      <span className="sr-only" role="status" aria-live="polite">
        {announcement}
      </span>
    </div>
  );
}
