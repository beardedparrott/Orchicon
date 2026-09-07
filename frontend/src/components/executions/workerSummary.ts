// Pure parser for the ORCHICON WORKER SUMMARY trailer that workers emit
// at the tail of their execution Output blob.
//
// Contract (see internal/db/seed_workers.go + the worker system prompt):
//
//   ORCHICON WORKER SUMMARY: <status> — <text>
//   FACTS LEARNED: <fact one>
//   FACTS LEARNED: <fact two>
//
// The marker is a text convention inside the execution `Output` field —
// there is no derived backend field. We parse it on the frontend so List
// views can badge executions without fetching the full Output blob, and
// so the execution page can surface the summary as a first-class card
// regardless of whether the session transcript / event stream is present
// (a native-`orchicon`-bridge run has Conversation == [] and sparse
// events, so the summary would otherwise sit unseen in Output).
//
// Status spellings are normalized: `success` / `failure` (the canonical
// routing words) plus common variants (`succeeded`, `failed`, `error`).
// Anything else is treated as an unknown status but still surfaces the
// summary text so the operator can read it.

export type WorkerSummaryStatus = "success" | "failure" | "unknown";

export interface WorkerSummary {
  /** true when the Output contains the ORCHICON WORKER SUMMARY marker. */
  present: boolean;
  /** Normalized status: success | failure | unknown. */
  status: WorkerSummaryStatus;
  /** The raw status token as emitted (e.g. "success", "succeeded"). */
  rawStatus: string;
  /** The summary text after the em-dash (may be empty). */
  text: string;
  /** FACTS LEARNED lines that follow the marker (may be empty). */
  factsLearned: string[];
}

const MARKER = "ORCHICON WORKER SUMMARY:";
const FACTS_PREFIX = "FACTS LEARNED:";

const SUCCESS_WORDS = new Set(["success", "succeeded", "ok", "pass", "passed"]);
const FAILURE_WORDS = new Set(["failure", "failed", "fail", "error", "errored"]);

function normalizeStatus(raw: string): WorkerSummaryStatus {
  const word = raw.trim().toLowerCase().replace(/[^a-z]/g, "");
  if (SUCCESS_WORDS.has(word)) return "success";
  if (FAILURE_WORDS.has(word)) return "failure";
  return "unknown";
}

/**
 * Parse the worker-summary trailer out of an execution Output blob.
 * Returns { present: false } when no marker is found.
 */
export function parseWorkerSummary(output: string | undefined | null): WorkerSummary {
  if (!output) {
    return { present: false, status: "unknown", rawStatus: "", text: "", factsLearned: [] };
  }

  const markerIdx = output.indexOf(MARKER);
  if (markerIdx === -1) {
    return { present: false, status: "unknown", rawStatus: "", text: "", factsLearned: [] };
  }

  // Everything from the marker to the end of the blob is the summary
  // trailer. Split into lines; the first line carries the status + text,
  // subsequent FACTS LEARNED lines are collected.
  const trailer = output.slice(markerIdx + MARKER.length);
  const lines = trailer.split(/\r?\n/);

  const first = lines[0] ?? "";
  // Status is the first whitespace-delimited token; the rest (after an
  // optional em-dash) is the summary text.
  const trimmed = first.trim();
  const spaceIdx = trimmed.search(/\s/);
  const rawStatus = spaceIdx === -1 ? trimmed : trimmed.slice(0, spaceIdx);
  let text = spaceIdx === -1 ? "" : trimmed.slice(spaceIdx).trim();
  // Strip a leading em-dash separator (— or -) from the text.
  text = text.replace(/^[—–-]\s*/, "").trim();

  const factsLearned: string[] = [];
  for (let i = 1; i < lines.length; i++) {
    const line = lines[i].trim();
    if (line.startsWith(FACTS_PREFIX)) {
      const fact = line.slice(FACTS_PREFIX.length).trim();
      if (fact) factsLearned.push(fact);
    }
  }

  return {
    present: true,
    status: normalizeStatus(rawStatus),
    rawStatus,
    text,
    factsLearned,
  };
}
