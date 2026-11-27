// streamGate.test.ts — NO FINISHED RECORD MAY HOLD A STREAM CONNECTION.
//
// THE OPERATOR: "8/10 times I get a white screen that loads forever unless I first close out the
// execution and then it recovers. We need to do something to allow more sessions active and prevent
// any kind of hanging."
//
// WHY A GATE IS THE CAPACITY FIX. The plane serves no HTTP/2 — and a browser could not use h2c over
// cleartext anyway, so TLS would be needed before multiplexing is even available. Until then EVERY
// server-stream subscription costs one of the browser's ~6 connections per origin. Two halves close
// that: (1) a stream is ABORTED on unmount (api/useStream.ts, pinned by useStream.abort.test.ts), and
// (2) a stream is never OPENED for a record that has finished — which is this file.
//
// The gate has to hold in every stream-holding route, keyed to that record's own terminal status. The
// sweep at the bottom is the part that matters long-term: a new live page added without a gate would
// reintroduce the exhaustion, silently, one connection at a time.

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

import {
  executionStreamEnabled,
  recoveryIsTerminal,
  recordStreamEnabled,
  workflowRunIsTerminal,
} from "@/lib/debouncedInvalidation";

const read = (rel: string) => readFileSync(resolve(process.cwd(), rel), "utf8");

describe("the shared stream gate", () => {
  it("requires an id and a non-terminal record", () => {
    expect(recordStreamEnabled("r1", false)).toBe(true);
    expect(recordStreamEnabled("r1", true)).toBe(false);
    expect(recordStreamEnabled("", false)).toBe(false);
    expect(recordStreamEnabled("", true)).toBe(false);
  });

  it("agrees with the execution gate it generalises", () => {
    // executionStreamEnabled is the original; recordStreamEnabled must not drift from it.
    for (const [id, terminal] of [
      ["e1", false],
      ["e1", true],
      ["", false],
      ["", true],
    ] as const) {
      expect(recordStreamEnabled(id, terminal)).toBe(executionStreamEnabled(id, terminal));
    }
  });
});

describe("terminal status sets", () => {
  // proto workflow.proto:211 — COMPLETED 3, FAILED 4, ABORTED 5.
  it("workflow runs: completed/failed/aborted are terminal, PAUSED is not", () => {
    for (const s of [3, 4, 5]) {
      expect(workflowRunIsTerminal(s), `run status ${s} must be terminal`).toBe(true);
    }
    // PAUSED (6) is resumable, and both in-flight states must stay live.
    for (const s of [0, 1, 2, 6]) {
      expect(workflowRunIsTerminal(s), `run status ${s} must stay live`).toBe(false);
    }
    // An unfetched run must not count as terminal, or the stream would never open on load.
    expect(workflowRunIsTerminal(undefined)).toBe(false);
  });

  // proto recovery.proto:141 — RESUMED 3, FAILED 5, CANCELLED 6.
  it("recoveries: resumed/failed/cancelled are terminal, ESCALATED and BLOCKED are not", () => {
    for (const s of [3, 5, 6]) {
      expect(recoveryIsTerminal(s), `recovery status ${s} must be terminal`).toBe(true);
    }
    // ESCALATED (4) and BLOCKED (7) both AWAIT A HUMAN — these are precisely the pages an operator
    // leaves open, so killing their stream would be the opposite of useful.
    for (const s of [0, 1, 2, 4, 7]) {
      expect(recoveryIsTerminal(s), `recovery status ${s} must stay live`).toBe(false);
    }
    expect(recoveryIsTerminal(undefined)).toBe(false);
  });
});

describe("every stream-holding route is gated", () => {
  // THE SWEEP. Each route that opens a server stream must pass an `enabled:` gate built from the shared
  // helper. Adding a live page without one is how the connection pool gets exhausted again.
  const ROUTES: Array<[string, string]> = [
    ["src/routes/executions_.$id.tsx", "executionStreamEnabled"],
    ["src/routes/workflows_.$id_.runs.$runId.tsx", "recordStreamEnabled"],
    ["src/routes/recovery_.$id.tsx", "recordStreamEnabled"],
  ];

  for (const [rel, helper] of ROUTES) {
    it(`${rel} gates its stream on the record's terminal state`, () => {
      const src = read(rel);
      expect(src, `${rel} does not import ${helper}`).toContain(helper);
      expect(
        src,
        `${rel} opens a stream without an \`enabled:\` gate — a finished record would hold a browser ` +
          `connection for the life of the tab`,
      ).toMatch(/enabled:\s*[^,\n]*\(/);
    });
  }

  // The project page's stream is keyed to the PROJECT, which has no run/recovery lifecycle — a project
  // is never "finished" — so it is deliberately ungated. This states that rather than leaving it as an
  // unexplained exception, so a later reader does not "fix" a non-bug or copy the omission elsewhere.
  it("the project page's stream is lifecycle-free by design, not by omission", () => {
    const src = read("src/routes/projects_.$id.tsx");
    expect(src).toContain("useStreamProjectEvents");
    expect(
      src,
      "if the project stream gains a gate, update this note and the ROUTES sweep together",
    ).not.toMatch(/enabled:\s*recordStreamEnabled/);
  });
});
