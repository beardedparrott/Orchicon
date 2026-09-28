import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { ChevronDown, ShieldOff, ShieldCheck } from "lucide-react";
import { cn } from "@/lib/utils";

interface FullsendToggleProps {
  /** on is the SERVER's answer (Conversation.fullsend), never this client's last write. */
  on: boolean;
  onChange: (on: boolean) => void;
  disabled?: boolean;
  className?: string;
}

// FullsendToggle — the composer's FULLSEND control, rendered immediately LEFT of the
// conversation-mode dropdown (the operator's placement: "a drop down to the left of the ask
// mode drop down").
//
// WHY IT IS A DROPDOWN AND NOT A SWITCH. A switch reads as "a preference", and this is not
// one: it is a waiver of the permission prompt, and the operator should have to choose ON
// from a named pair rather than nudge a control. It also mirrors the mode dropdown beside it,
// so the two states of the composer read as the same kind of thing.
//
// THE BOUNDARY IS STATED IN THE CONTROL, not only in a tooltip. The denial list and the
// never-allow binary class (sudo / dd / mkfs*) still refuse while fullsend is on, and an
// operator who believes "allow everything" opened their own exclusions has been misled about
// their own policy — the one thing a consent surface must not do.
//
// IT IS LOUD WHEN ON, and silent when off: the trigger fills with the destructive colour and
// names the state, because this is the only thing on screen that says Orchicon has stopped
// asking. A mode the operator cannot tell they are in is worse than no mode.
export function FullsendToggle({
  on,
  onChange,
  disabled,
  className,
}: FullsendToggleProps) {
  const [open, setOpen] = useState(false);
  const [announcement, setAnnouncement] = useState("");
  const ref = useRef<HTMLDivElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const [menuStyle, setMenuStyle] = useState<React.CSSProperties>({});

  // Position the portaled menu ABOVE the trigger. The composer sits at the bottom of the
  // viewport, so a menu opened downwards would be off-screen — the same reason ModeToggle
  // portals with a `bottom` anchor.
  useEffect(() => {
    if (open && ref.current) {
      const rect = ref.current.getBoundingClientRect();
      setMenuStyle({
        position: "fixed",
        bottom: `${window.innerHeight - rect.top + 4}px`,
        right: `${window.innerWidth - rect.right}px`,
        minWidth: `${Math.max(rect.width, 220)}px`,
      });
    }
  }, [open]);

  const choose = useCallback(
    (next: boolean) => {
      if (disabled) return;
      setOpen(false);
      // NOTHING IS ANNOUNCED AS DONE THAT THE SERVER HAS NOT CONFIRMED. The state comes back
      // on the conversation row, so the announcement reports what is being REQUESTED and the
      // control re-renders from the server's answer. A client that flipped its own label
      // first would show "on" for a write that failed.
      if (next === on) {
        setAnnouncement(`Fullsend is already ${next ? "on" : "off"}`);
        return;
      }
      setAnnouncement(
        next
          ? "Fullsend on. Orchicon will stop asking for permission, and any permission card already on screen is approved."
          : "Fullsend off. Orchicon will ask for permission again.",
      );
      onChange(next);
    },
    [disabled, on, onChange],
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
        case "ArrowUp":
          e.preventDefault();
          if (!open) setOpen(true);
          else choose(!on);
          break;
        case "Enter":
        case " ":
          e.preventDefault();
          setOpen((v) => !v);
          break;
      }
    },
    [disabled, open, on, choose],
  );

  const options: { value: boolean; label: string; blurb: string }[] = [
    {
      value: false,
      label: "Off",
      blurb: "Ask before a write or an execution, as usual",
    },
    {
      value: true,
      label: "On — stop asking",
      blurb:
        "Proceed without asking for permission in this conversation, and approve a permission card already on screen. Your deny list and the never-allow class (sudo / dd / mkfs*) still refuse.",
    },
  ];

  return (
    <div ref={ref} className={cn("relative", className)}>
      <button
        type="button"
        role="combobox"
        aria-expanded={open}
        aria-label={on ? "Fullsend is on" : "Fullsend is off"}
        aria-haspopup="listbox"
        title={
          on
            ? "FULLSEND is ON: Orchicon is not asking for permission in this conversation. Your deny list and the never-allow class (sudo / dd / mkfs*) still refuse — this waives the PROMPT, not your policy. A question still waits for your answer."
            : "Fullsend is off: Orchicon asks before a write or an execution"
        }
        disabled={disabled}
        onClick={() => setOpen((v) => !v)}
        onKeyDown={handleKeyDown}
        className={cn(
          "inline-flex items-center gap-1 rounded-md glass-input px-2 py-1.5 text-xs font-medium transition-colors",
          "focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring",
          "disabled:opacity-50 disabled:pointer-events-none",
          on
            ? // LOUD: a filled destructive chip, not a tint. This is the one control whose
              // "on" state has to survive a glance.
              "border-destructive/60 bg-destructive text-destructive-foreground hover:bg-destructive/90"
            : "hover:bg-accent hover:text-accent-foreground",
        )}
        data-testid="fullsend-toggle"
        data-fullsend={on ? "on" : "off"}
      >
        {on ? (
          <ShieldOff aria-hidden="true" className="h-3.5 w-3.5" />
        ) : (
          <ShieldCheck aria-hidden="true" className="h-3.5 w-3.5 text-muted-foreground" />
        )}
        <span className="hidden sm:inline">{on ? "FULLSEND" : "Fullsend"}</span>
        <ChevronDown
          aria-hidden="true"
          className={cn(
            "h-3 w-3 transition-transform",
            on ? "text-destructive-foreground/80" : "text-muted-foreground",
            open && "rotate-180",
          )}
        />
      </button>
      {open &&
        createPortal(
          <div
            ref={menuRef}
            role="listbox"
            aria-label="Fullsend"
            style={menuStyle}
            className="z-50 overflow-hidden rounded-xl glass-menu text-popover-foreground animate-in fade-in-0 zoom-in-95"
            data-testid="fullsend-menu"
          >
            {options.map((opt) => {
              const active = on === opt.value;
              return (
                <button
                  key={String(opt.value)}
                  role="option"
                  aria-selected={active}
                  type="button"
                  title={opt.blurb}
                  onClick={() => choose(opt.value)}
                  className={cn(
                    "flex w-full flex-col items-start gap-0.5 px-3 py-2 text-left text-xs transition-colors",
                    "hover:bg-accent hover:text-accent-foreground",
                    "focus:bg-accent focus:text-accent-foreground focus:outline-none",
                    active && "bg-accent text-accent-foreground",
                  )}
                >
                  <span className="flex w-full items-center gap-2 font-medium">
                    {opt.label}
                    {active && <span className="ml-auto text-primary">✓</span>}
                  </span>
                  {/* THE BOUNDARY, IN THE OPTION ITSELF: an operator choosing ON is entitled
                      to see what it does NOT cover before they choose it. */}
                  <span className="text-[10px] font-normal text-muted-foreground">
                    {opt.blurb}
                  </span>
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
