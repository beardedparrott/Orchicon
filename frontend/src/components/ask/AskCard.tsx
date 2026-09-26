import { useRef, useState } from "react";
import type { KeyboardEvent as ReactKeyboardEvent, ReactNode } from "react";
import { HelpCircle, ShieldAlert } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  PermissionChoice,
  type PermissionAsk,
} from "@/api/gen/orchicon/api/v1/ask_orchicon_service_pb";
import { askTargetLabel, outcomeLabel, type AskOutcome } from "@/lib/ask-consent";

// AskCard — the shared card primitive for the two things a turn can ask.
//
// ONE CARD, TWO USES, TWO BLOCKING MODELS. `AskCard` renders a recorded
// `ask_user` tool call as a card whose options the operator can click; the click
// sends the choice as a NORMAL user message (via the caller's existing send
// path). `ConsentAskCard` renders a CONSENT ask — an approval flowing back over
// the transport — on the SAME shell. A CLARIFYING question is RECORDED and the
// turn ends (the user answers in their next message), while a CONSENT ask is
// genuinely BLOCKING on the transport, so only the consent card is a pending
// item with a real reply RPC. Both render through `AskCardShell`, which is the
// one place that owns the keyboard model: arrow keys rove focus across the
// `[data-ask-action]` buttons, Enter/Space activate (native button behaviour),
// and ESCAPE MEANS DENY on a consent card — never a silent dismissal, which
// would leave the turn waiting with no way back.
//
// NEITHER CARD IS A MODAL. A consent ask is a pending transcript row: the
// composer, the sidebar and Stop stay live, because the turn is opencode's and
// is not blocked by us.

export interface AskCardOption {
  label: string;
  description?: string;
}

export interface AskCardProps {
  question: string;
  options: AskCardOption[];
  /** allowOther hints that free text is an acceptable answer too. */
  allowOther?: boolean;
  /** answered=true renders a settled, non-clickable card (a later message exists). */
  answered?: boolean;
  /** error renders the compact "unparseable call" state instead of options. */
  error?: string;
  /** onSelect fires with the chosen option's label; the caller sends it as a user message. */
  onSelect?: (label: string) => void;
  className?: string;
}

/**
 * parseAskUserArgs parses a recorded ask_user call's JSON `arguments`.
 * It NEVER throws: a malformed payload returns null, so the caller renders the
 * card's error state rather than crashing the transcript.
 */
export function parseAskUserArgs(
  raw: string | undefined,
): { question: string; options: AskCardOption[]; allowOther: boolean } | null {
  if (!raw) return null;
  let obj: unknown;
  try {
    obj = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!obj || typeof obj !== "object") return null;
  const rec = obj as Record<string, unknown>;
  const question = typeof rec.question === "string" ? rec.question : "";
  if (!question.trim()) return null;
  const options: AskCardOption[] = [];
  if (Array.isArray(rec.options)) {
    for (const o of rec.options) {
      if (typeof o === "string") {
        if (o.trim()) options.push({ label: o });
        continue;
      }
      if (o && typeof o === "object") {
        const or = o as Record<string, unknown>;
        const label = typeof or.label === "string" ? or.label : "";
        if (!label.trim()) continue;
        options.push({
          label,
          description: typeof or.description === "string" ? or.description : undefined,
        });
      }
    }
  }
  return { question, options, allowOther: rec.allow_other === true };
}

/**
 * isAskUserToolCall reports whether a recorded tool call is the clarifying
 * question this card renders (tolerating the `orchicon_` MCP-style prefix the
 * model may emit).
 */
export function isAskUserToolCall(functionName: string): boolean {
  return functionName === "ask_user" || functionName === "orchicon_ask_user";
}

// --- the shared shell -----------------------------------------------------

export interface AskCardShellProps {
  /** header is the small uppercase line naming which kind of ask this is. */
  header: string;
  /** tone colours the shell: a consent ask is not a clarification. */
  tone?: "question" | "consent";
  /** answered marks the card as settled (a resolved consent ask, an answered question). */
  answered?: boolean;
  /**
   * onEscape runs when Escape is pressed with focus inside the card. When it is
   * NOT provided the event is left alone, so a document-level handler can still
   * treat Escape as Deny for an outstanding consent ask. A card never dismisses
   * itself: only the caller decides what Escape means here.
   */
  onEscape?: () => void;
  className?: string;
  children: ReactNode;
}

/**
 * AskCardShell is the ONE card container both variants render through — header,
 * body, action column, and the keyboard model. Two components rendering the
 * same shape is how two cards drift apart, so this owns the shape.
 */
export function AskCardShell({
  header,
  tone = "question",
  answered = false,
  onEscape,
  className,
  children,
}: AskCardShellProps) {
  const ref = useRef<HTMLDivElement | null>(null);

  // Arrow keys move a roving focus across this card's actions; Enter/Space are
  // native button activation. Focus stays INSIDE the card, so an operator can
  // answer without a mouse and without a global key registry.
  const onKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Escape") {
      if (!onEscape) return;
      e.preventDefault();
      onEscape();
      return;
    }
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    const buttons = Array.from(
      ref.current?.querySelectorAll<HTMLButtonElement>(
        "[data-ask-action]:not([disabled])",
      ) ?? [],
    );
    if (buttons.length === 0) return;
    const at = buttons.indexOf(document.activeElement as HTMLButtonElement);
    const next =
      e.key === "ArrowDown"
        ? at < 0
          ? 0
          : (at + 1) % buttons.length
        : at <= 0
          ? buttons.length - 1
          : at - 1;
    e.preventDefault();
    buttons[next]?.focus();
  };

  return (
    <div
      ref={ref}
      onKeyDown={onKeyDown}
      className={cn(
        "max-w-[88%] rounded-2xl rounded-tl-sm border px-4 py-3",
        tone === "consent"
          ? "border-amber-500/40 bg-amber-500/5"
          : "border-primary/30 bg-primary/5",
        className,
      )}
      data-testid="ask-card"
      data-answered={answered ? "true" : "false"}
      data-ask-tone={tone}
    >
      <div className="mb-2 flex items-center gap-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
        {tone === "consent" ? (
          <ShieldAlert aria-hidden="true" className="h-3.5 w-3.5" />
        ) : (
          <HelpCircle aria-hidden="true" className="h-3.5 w-3.5" />
        )}
        {header}
      </div>
      {children}
    </div>
  );
}

export interface AskCardActionProps {
  children: ReactNode;
  /** hint is the secondary line under the label (what the action covers). */
  hint?: string;
  disabled?: boolean;
  tone?: "default" | "primary" | "danger";
  onSelect?: () => void;
  testId?: string;
}

/**
 * AskCardAction is one card action: a real <button> (so Enter/Space work
 * natively) carrying `data-ask-action` for the shell's roving focus.
 */
export function AskCardAction({
  children,
  hint,
  disabled = false,
  tone = "default",
  onSelect,
  testId,
}: AskCardActionProps) {
  const toneClass = disabled
    ? "cursor-default border-border bg-background/50 text-muted-foreground"
    : tone === "primary"
      ? "border-primary/50 bg-primary/10 hover:bg-primary/20"
      : tone === "danger"
        ? "border-destructive/50 bg-destructive/5 text-destructive hover:bg-destructive/15"
        : "border-border bg-background hover:border-primary hover:bg-primary/10";
  return (
    <button
      type="button"
      data-ask-action=""
      data-testid={testId}
      disabled={disabled}
      onClick={() => onSelect?.()}
      className={cn(
        "rounded-lg border px-3 py-2 text-left text-sm transition-colors",
        toneClass,
      )}
    >
      <span className="font-medium">{children}</span>
      {hint && <span className="block text-xs text-muted-foreground">{hint}</span>}
    </button>
  );
}

/** AskCardActions is the action column, in the order the actions are given. */
export function AskCardActions({ children }: { children: ReactNode }) {
  return <div className="flex flex-col gap-1.5">{children}</div>;
}

// --- the clarifying-question variant --------------------------------------

/**
 * AskCard renders a clarifying question: the question, its options, and — when
 * the model allowed it — a free-text "Other". Selecting sends the choice as the
 * NEXT USER MESSAGE (the topic is not blocked; the answer arrives as a normal
 * turn), which is why this card never calls an RPC.
 */
export function AskCard({
  question,
  options,
  allowOther = false,
  answered = false,
  error,
  onSelect,
  className,
}: AskCardProps) {
  const [otherOpen, setOtherOpen] = useState(false);
  const [otherText, setOtherText] = useState("");
  const interactive = !answered && !!onSelect;

  return (
    <AskCardShell
      header={answered ? "Question (answered)" : "Orchicon is asking"}
      answered={answered}
      className={className}
      // Escape collapses an OPEN free-text row and does nothing otherwise: a
      // clarifying question must never be settled by a keystroke (nothing is
      // waiting on us for it). With no row open the event is deliberately left
      // un-prevented, so a document-level handler can still treat Escape as
      // Deny for an outstanding consent ask.
      onEscape={otherOpen ? () => setOtherOpen(false) : undefined}
    >
      {error ? (
        <p className="text-sm text-destructive" data-testid="ask-card-error">
          Could not read this clarifying question ({error}).
        </p>
      ) : (
        <>
          <p className="text-sm mb-3 whitespace-pre-wrap [overflow-wrap:anywhere]">{question}</p>
          <AskCardActions>
            {options.map((o, i) => (
              <AskCardAction
                key={`${o.label}-${i}`}
                testId="ask-card-option"
                disabled={answered || !onSelect}
                hint={o.description}
                onSelect={() => onSelect?.(o.label)}
              >
                {o.label}
              </AskCardAction>
            ))}
            {allowOther && interactive &&
              (otherOpen ? (
                <div
                  className="rounded-lg border border-dashed border-border px-3 py-2"
                  data-testid="ask-card-other-row"
                >
                  <input
                    autoFocus
                    data-testid="ask-card-other"
                    aria-label="Other answer"
                    placeholder="Type your answer…"
                    value={otherText}
                    onChange={(e) => setOtherText(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" && otherText.trim()) {
                        const answer = otherText.trim();
                        setOtherOpen(false);
                        setOtherText("");
                        onSelect?.(answer);
                      }
                    }}
                    className="w-full bg-transparent text-sm outline-none"
                  />
                  <span className="text-xs text-muted-foreground">
                    Enter sends it as your next message · Escape cancels
                  </span>
                </div>
              ) : (
                <AskCardAction
                  testId="ask-card-other-open"
                  hint="Answer in your own words — sent as your next message"
                  onSelect={() => setOtherOpen(true)}
                >
                  Other…
                </AskCardAction>
              ))}
          </AskCardActions>
          {allowOther && !interactive && (
            <p className="mt-2 text-xs text-muted-foreground">
              You can also answer in your own words below.
            </p>
          )}
        </>
      )}
    </AskCardShell>
  );
}

// --- the consent variant --------------------------------------------------

export interface ConsentAskCardProps {
  ask: PermissionAsk;
  /** outcome is null while the ask is still awaiting a decision. */
  outcome?: AskOutcome | null;
  /** busy disables the actions while the reply is in flight. */
  busy?: boolean;
  onDecide: (choice: PermissionChoice) => void;
  /** onEscape is wired by the caller to Deny — never to a dismissal. */
  onEscape?: () => void;
  className?: string;
}

/**
 * ConsentAskCard renders one pending consent ask: the tool, the TARGET (the
 * path for a write, the command for bash), the directory a session grant would
 * cover, and the three actions. A SETTLED card (outcome set) renders the
 * transcript line instead, so scrolling back shows that a grant was given and
 * what it covered.
 */
export function ConsentAskCard({
  ask,
  outcome = null,
  busy = false,
  onDecide,
  onEscape,
  className,
}: ConsentAskCardProps) {
  const settled = outcome !== null;
  const denyBelow = ask.denyEntriesBelow ?? [];
  // A SETTLED ask is a ONE-LINE RECORD, not a card. The card is for a decision
  // still to be made; once made it is history, and a full tinted block per past
  // grant buries the live turn under its own audit trail. This is what the TUI
  // already does (internal/tui/chat/consent_render.go: card while pending, a
  // one-line record once decided); the GUI had not adopted that split. The
  // outcome TEXT is unchanged, so scrolling back still shows that a grant was
  // given and exactly what it covered.
  if (settled) {
    return (
      <p
        className={cn(
          "text-xs text-muted-foreground [overflow-wrap:anywhere]",
          className,
        )}
        data-testid="consent-ask-outcome"
        data-answered="true"
      >
        {outcomeLabel(ask, outcome)}
      </p>
    );
  }
  return (
    <AskCardShell
      header="Permission needed"
      tone="consent"
      className={className}
      onEscape={onEscape}
    >
      {/* The target, named — never just an opaque id. */}
      <p
        className="text-sm mb-1 whitespace-pre-wrap [overflow-wrap:anywhere]"
        data-testid="consent-ask-target"
      >
        {askTargetLabel(ask)}
      </p>
      <p className="text-xs text-muted-foreground [overflow-wrap:anywhere]" data-testid="consent-ask-tool">
        {ask.tool}
        {ask.directory ? ` · ${ask.directory}` : ""}
        {ask.insideProject ? " · inside this conversation's project" : ""}
      </p>
      {denyBelow.length > 0 && (
        <p
          className="mt-2 text-xs text-muted-foreground [overflow-wrap:anywhere]"
          data-testid="consent-ask-deny-note"
        >
          A session grant never overrides your deny list: {denyBelow.join(", ")}{" "}
          still refuses its own paths here, so this grant covers the rest of the
          directory only.
        </p>
      )}
      <AskCardActions>
        <AskCardAction
          testId="consent-ask-allow-once"
          tone="primary"
          disabled={busy}
          hint="Proceed for this one call — the same call asks again"
          onSelect={() => onDecide(PermissionChoice.ALLOW_ONCE)}
        >
          Allow once
        </AskCardAction>
        <AskCardAction
          testId="consent-ask-allow-session"
          disabled={busy}
          hint={
            ask.directory
              ? `Covers everything under ${ask.directory} this session`
              : "Covers this for the rest of the conversation"
          }
          onSelect={() => onDecide(PermissionChoice.ALLOW_SESSION)}
        >
          Allow for this session
        </AskCardAction>
        <AskCardAction
          testId="consent-ask-deny"
          tone="danger"
          disabled={busy}
          hint="Refuse this call (Escape)"
          onSelect={() => onDecide(PermissionChoice.DENY)}
        >
          Deny
        </AskCardAction>
      </AskCardActions>
      <p className="mt-2 text-xs text-muted-foreground">
        Escape denies. Nothing is blocked while you decide — this card is a
        pending item, not a modal.
      </p>
    </AskCardShell>
  );
}

export default AskCard;
