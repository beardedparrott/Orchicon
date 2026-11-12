/**
 * adapter-errors.ts — turn an adapter RPC failure into something an operator can ACT on.
 *
 * The operator, verbatim:
 *
 *   'When choosing opencode and the user doesn't have opencode installed, it says "Failed to load
 *    providers: ConnectError: [unimplemented] model discovery is not configured (set
 *    ORCHICON_OPENCODE_BIN or install opencode on PATH). Failed to load models: ConnectError:
 *    [unimplemented] model discovery is not configured (set ORCHICON_OPENCODE_BIN or install
 *    opencode on PATH)" I think a more graceful approach of saying something like "Please ensure you
 *    have opencode installed to use this provider"'
 *
 * They are right, and the shape of the mistake is worth naming: the picker rendered
 * `String(error)` — the TRANSPORT error, with its Connect code, its Go error wrapping and a
 * variable name — twice, once per tier. Everything in that string is true and none of it is a
 * sentence a person can act on: "ConnectError: [unimplemented]" describes the RPC, not the problem,
 * and the remedy is buried in a parenthetical about a build-time path.
 *
 * WHAT THE OPERATOR ACTUALLY NEEDS TO KNOW, in order:
 *   1. WHAT is missing (the opencode CLI, on the plane's host).
 *   2. WHY this adapter needs it (opencode's models are discovered by running its CLI).
 *   3. WHAT to do instead, right now (use the native "orchicon" adapter — no binary required).
 *
 * So this maps the known failure onto a short, actionable sentence, and keeps the raw text reachable
 * as a `detail` for the rare case where the message is something new. Unknown failures are NOT
 * swallowed into a vague apology: they fall through to their own message, because inventing a
 * friendly sentence for an unclassified error is how a real bug becomes invisible.
 */

/** A failure rendered for a human: a sentence, and the raw text kept for diagnosis. */
export interface AdapterErrorText {
  /** One actionable sentence. Never contains a Connect code or a Go error wrapper. */
  message: string;
  /** The raw error text, for a tooltip / expander. Empty when the message says it all. */
  detail: string;
}

/** The adapter kinds whose model tier is discovered by shelling out to the opencode CLI. */
const OPencodeDiscoveryKinds = ["opencode"];

/** Does this error say model discovery is unavailable? */
function isDiscoveryUnconfigured(raw: string): boolean {
  return /model discovery is not configured/i.test(raw);
}

/** Does this error look like the opencode binary is absent? */
function isOpencodeMissing(raw: string): boolean {
  return (
    /ORCHICON_OPENCODE_BIN/.test(raw) ||
    /install opencode on PATH/i.test(raw) ||
    /opencode.*not found/i.test(raw)
  );
}

/**
 * translateAdapterError renders an adapter-tier RPC failure for display.
 *
 * `tier` picks the wording: a provider-tier failure and a model-tier failure are the same underlying
 * cause on an opencode-free plane, and saying it twice in two different ways (the reported symptom)
 * is worse than saying it once.
 */
export function translateAdapterError(
  error: unknown,
  adapter: string,
  tier: "providers" | "models",
): AdapterErrorText {
  const raw = rawText(error);

  // THE REPORTED CASE: opencode selected, no binary on the plane.
  if (isDiscoveryUnconfigured(raw) || isOpencodeMissing(raw)) {
    const needsOpencode = OPencodeDiscoveryKinds.includes(adapter);
    if (needsOpencode) {
      return {
        message:
          "This adapter needs the opencode CLI installed on the Orchicon host. " +
          "Use the “orchicon” adapter instead — it needs nothing installed — " +
          "or install opencode and try again.",
        detail: "",
      };
    }
    // A NON-opencode adapter should never take the CLI discovery path; if it does, that is a real
    // bug and the raw text is the only useful thing to show.
    return { message: `Could not list ${tier} for “${adapter}”.`, detail: raw };
  }

  // Anything unclassified keeps its own words. A friendly sentence invented for an unknown failure
  // hides the failure; the raw text is what a bug report needs.
  return { message: raw, detail: "" };
}

/** rawText flattens whatever came back into one line, without the ConnectError prefix. */
function rawText(error: unknown): string {
  const s =
    error instanceof Error
      ? error.message
      : typeof error === "string"
        ? error
        : (() => {
            try {
              return JSON.stringify(error);
            } catch {
              return String(error);
            }
          })();
  // "ConnectError: [unimplemented] <the actual reason>" -> "<the actual reason>". The prefix names
  // the transport, which is never the operator's problem.
  return s.replace(/^ConnectError:\s*(\[[^\]]*\]\s*)?/i, "").trim();
}
