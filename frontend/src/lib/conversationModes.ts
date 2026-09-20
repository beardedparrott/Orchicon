// conversationModes.ts — the Ask Orchicon MODE vocabulary, in ONE place for the GUI.
//
// The operator: "The mode drop down for ask orchicon in the gui does not include iteration or quick work mode."
//
// It did not, because the dropdown's option list was a hardcoded single-element array written when ONE mode
// existed, and it was never revisited when the other two landed. The same assumption was in two more places
// (the dropdown's screen-reader announcement, and the conversation header's mode pill, which displayed the word
// "Brainstorm" on every conversation whatever its real mode). All three are fixed by this module, whose point
// is the DERIVATION: the list comes from the proto enum rather than from a literal, so the next mode to land
// appears instead of being silently absent.
//
// IT IMPORTS NOTHING FROM REACT, deliberately. The icon map lives in the component that draws it
// (components/ui/ModeIcon.tsx) so this file stays pure and unit-testable — this repo's sidebar and metrics
// logic are split the same way, and for the same reason: a vocabulary that only a component can exercise is a
// vocabulary nothing checks.
//
// The TUI has carried all three modes since /mode learned them (internal/tui: `ModeNames`/`ParseMode`, and the
// composer pill's `currentModeLabel`), so this is the GUI's half of a parity gap, not a new feature.

import { ConversationMode } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";

/** ConversationModeMeta is what the UI needs to present one mode. */
export interface ConversationModeMeta {
  value: ConversationMode;
  /** The label the operator reads. Matches the TUI's own vocabulary ("quick work" lower-cased there, on a stat strip). */
  label: string;
  /** One line on what the mode does, for a tooltip. */
  blurb: string;
}

/**
 * MODE_META describes each mode, KEYED BY THE ENUM.
 *
 * A map rather than an ordered array on purpose: an array is a second list that can fall out of step with the
 * proto, and one falling out of step is precisely this bug. Written as a map, the only thing that can be
 * missing is a DESCRIPTION, and an undescribed mode still appears (see CONVERSATION_MODES).
 */
const MODE_META: Record<number, Omit<ConversationModeMeta, "value">> = {
  [ConversationMode.BRAINSTORM]: {
    label: "Brainstorm",
    blurb: "Plans: designs, researches, asks clarifying questions and authors the work.",
  },
  [ConversationMode.ITERATION]: {
    label: "Iteration",
    blurb: "Does the work: branches, edits, runs the tests and commits.",
  },
  [ConversationMode.QUICK_WORK]: {
    label: "Quick Work",
    blurb: "Dispatches it: an ephemeral worker and workflow, fired and cleaned up.",
  },
};

/**
 * CONVERSATION_MODES is every SELECTABLE mode, derived from the enum and ordered by its value.
 *
 * UNSPECIFIED IS EXCLUDED. It is the wire's "absent" value rather than a mode, and the server reads it as
 * BRAINSTORM — offering it would let an operator pick a state that is not a mode at all.
 *
 * A MODE WITH NO ENTRY IN MODE_META STILL APPEARS, labelled from the enum's own name. That fallback is the
 * whole point: a new mode shipped without its description is a cosmetic gap someone will notice and finish,
 * whereas a mode missing because a hardcoded list was not updated is invisible until an operator goes looking
 * for it — which is exactly how this was found.
 */
export const CONVERSATION_MODES: ConversationModeMeta[] = Object.values(ConversationMode)
  .filter((v): v is ConversationMode => typeof v === "number" && v !== ConversationMode.UNSPECIFIED)
  .sort((a, b) => a - b)
  .map((value) => {
    const described = MODE_META[value];
    if (described) return { value, ...described };
    // The reverse mapping a numeric TS enum carries: BRAINSTORM for 1, and so on.
    const fromEnum = ConversationMode[value];
    return { value, label: fromEnum || `mode ${value}`, blurb: "" };
  });

/** conversationModeMeta is the entry for a mode, or undefined when the mode is not selectable. */
export function conversationModeMeta(mode: ConversationMode): ConversationModeMeta | undefined {
  return CONVERSATION_MODES.find((m) => m.value === mode);
}

/**
 * conversationModeLabel names a mode for display.
 *
 * It falls back rather than blanking, the same chain the TUI's `currentModeLabel` uses: an unmapped but known
 * value reads as its enum name, and an outright unknown one as the raw number. A blank pill would be worse than
 * an ugly one — it would say the conversation has no mode, which is never true.
 */
export function conversationModeLabel(mode: ConversationMode): string {
  const known = conversationModeMeta(mode);
  if (known) return known.label;
  return ConversationMode[mode] || `mode ${mode}`;
}
