// Fixtures for worker-summary verification tests.
//
// 01M1WB5TP9RCF1740MT93HJKQS is the real execution observed in the work
// item: it succeeded (1.55M tokens) and DID emit `ORCHICON WORKER SUMMARY:
// success — …` + `FACTS LEARNED:` lines at the tail of its Output field,
// but the execution page session view appeared empty of any summary. These
// fixtures reproduce that shape so the parser/card tests cover the exact
// regression.

export const SUMMARY_SUCCESS_OUTPUT = `...earlier model output...

ORCHICON WORKER SUMMARY: success — DEDICATED Ask session runtime decision
FACTS LEARNED: the runtime container's supervisor runs the pre-feature daemon self-copy (old binary), so the sandbox plane does not auto-boot until the daemon rebuilds.
FACTS LEARNED: the execution page session view renders the event stream as chat; storedOutput only shows when messages.length === 0.`;

// A native-`orchicon`-bridge run has Conversation == [] and sparse/no
// events, but the Output still carries the summary marker. This is the
// empty-conversation + events-present case the card must cover.
export const SUMMARY_SUCCESS_EMPTY_CONVERSATION = `ORCHICON WORKER SUMMARY: success — DEDICATED Ask session runtime decision
FACTS LEARNED: native-bridge runs have Conversation == [] so the session view shows no transcript.`;

// An execution whose Output contains no summary marker at all.
export const NO_SUMMARY_OUTPUT = `Some ordinary model output with no worker summary trailer.`;

// A failure-status variant spelling.
export const SUMMARY_FAILURE_OUTPUT = `ORCHICON WORKER SUMMARY: failure — Found 3 bugs in the implementation.`;
