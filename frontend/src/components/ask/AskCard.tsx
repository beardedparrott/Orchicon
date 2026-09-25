import { HelpCircle } from "lucide-react";
import { cn } from "@/lib/utils";

// AskCard — the shared clarifying-question card primitive.
//
// ONE CARD, TWO USES, TWO BLOCKING MODELS. This renders a recorded `ask_user`
// tool call as a card whose options the operator can click; the click sends the
// choice as a NORMAL user message (via the caller's existing send path). The
// consent cards (an approval flowing back over the transport) reuse this same
// rendering primitive and the same reply-as-message pattern — but NOT the same
// blocking model: a CLARIFYING question is RECORDED and the turn ends (the user
// answers in their next message), while a CONSENT ask is genuinely BLOCKING on
// the transport. Keep that distinction when reusing this component.
//
// The card is interactive ONLY when nothing follows its assistant message in the
// transcript (`answered` false). Once a later message exists the question is
// settled — options are shown but not clickable — which is what makes "a turn
// that ends with an unanswered question is a COMPLETED turn" visible rather than
// a claim.

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

export function AskCard({
  question,
  options,
  allowOther = false,
  answered = false,
  error,
  onSelect,
  className,
}: AskCardProps) {
  return (
    <div
      className={cn(
        "max-w-[88%] rounded-2xl rounded-tl-sm border border-primary/30 bg-primary/5 px-4 py-3",
        className,
      )}
      data-testid="ask-card"
      data-answered={answered ? "true" : "false"}
    >
      <div className="mb-2 flex items-center gap-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
        <HelpCircle aria-hidden="true" className="h-3.5 w-3.5" />
        {answered ? "Question (answered)" : "Orchicon is asking"}
      </div>
      {error ? (
        <p className="text-sm text-destructive" data-testid="ask-card-error">
          Could not read this clarifying question ({error}).
        </p>
      ) : (
        <>
          <p className="text-sm mb-3 whitespace-pre-wrap [overflow-wrap:anywhere]">{question}</p>
          <div className="flex flex-col gap-1.5">
            {options.map((o, i) => (
              <button
                key={`${o.label}-${i}`}
                type="button"
                disabled={answered || !onSelect}
                onClick={() => onSelect?.(o.label)}
                className={cn(
                  "rounded-lg border px-3 py-2 text-left text-sm transition-colors",
                  answered || !onSelect
                    ? "cursor-default border-border bg-background/50 text-muted-foreground"
                    : "border-border bg-background hover:border-primary hover:bg-primary/10",
                )}
              >
                <span className="font-medium">{o.label}</span>
                {o.description && (
                  <span className="block text-xs text-muted-foreground">{o.description}</span>
                )}
              </button>
            ))}
          </div>
          {allowOther && (
            <p className="mt-2 text-xs text-muted-foreground">
              You can also answer in your own words below.
            </p>
          )}
        </>
      )}
    </div>
  );
}

export default AskCard;
