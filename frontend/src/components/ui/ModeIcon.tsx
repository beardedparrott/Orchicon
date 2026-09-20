// ModeIcon.tsx — the glyph for an Ask Orchicon mode.
//
// It exists so the ICON lives in exactly one place. Two components draw a mode (the composer's dropdown and the
// conversation header's pill), and the header pill is one of the three spots that had a hardcoded single-mode
// assumption baked in. A second icon map in the other component would be the same kind of duplicated literal
// that caused this, so the map is here and both callers use it.
//
// It is a COMPONENT rather than an export of the map because lib/conversationModes.ts is deliberately free of
// React imports so its vocabulary can be unit-tested; keeping the components out of that file is what preserves
// that property.

import { Brain, Hammer, Zap, type LucideIcon } from "lucide-react";

import { ConversationMode } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";

const ICONS: Record<number, LucideIcon> = {
  [ConversationMode.BRAINSTORM]: Brain,
  [ConversationMode.ITERATION]: Hammer,
  [ConversationMode.QUICK_WORK]: Zap,
};

/**
 * ModeIcon draws a mode's glyph.
 *
 * An unmapped mode falls back to Brain rather than rendering nothing: the same reasoning as the label fallback
 * in conversationModes.ts — a mode the UI has not been told about should look unfinished, not invisible.
 */
export function ModeIcon({ mode, className }: { mode: ConversationMode; className?: string }) {
  const Icon = ICONS[mode] ?? Brain;
  return <Icon aria-hidden="true" className={className} />;
}
