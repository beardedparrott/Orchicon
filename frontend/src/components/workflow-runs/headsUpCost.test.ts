// A HEADS-UP TILE MUST NOT DOWNLOAD THE WHOLE TRANSCRIPT.
//
// The operator, after clicking a run tile and opening the execution page:
//
//	"the GUI is hanging and if I do a hard refresh I just see a white screen … This
//	 feature is obviously causing some major performance problems with the
//	 webserver/site."
//
// The grid renders ONE TILE PER DAG STEP, and every tile fetched the FULL durable
// transcript (limit=10000) to feed extractLastTextBlock — which scans BACKWARDS from the
// end and returns at the first text part, usually within a handful. Measured against the
// real tenant: 175,024 parts on the execution, 10,000 returned (≈399 kB), and the tile read
// a 34-byte block. The execution page invalidated that shared query key on every 500 ms
// event burst for the length of the run, so every tile re-downloaded ~399 kB twice a second.
// The API saturated, the SPA chunk queued behind it (the page went white on refresh), and
// it recovered when the run went terminal and the burst stopped — the "hangs and then frees
// up on its own" that was reported.
//
// These are SOURCE-LEVEL assertions, which is unusual and deliberate: the failure mode is a
// constant in a call argument, and no unit test of the components would catch a
// re-introduced `limit: 10000` without standing up a full React Query harness with a mocked
// transport. The invariant — "a tile asks for a TAIL" — is decidable from the source, and
// this is the cheapest place that keeps it true.

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

const read = (rel: string) => readFileSync(resolve(process.cwd(), rel), "utf8");

const TILE = "src/components/workflow-runs/HeadsUpTile.tsx";
const PAGE = "src/routes/executions_.$id.tsx";
const API = "src/api/executions.ts";

describe("a Heads-Up tile reads a tail, not the transcript", () => {
  it("does NOT use the full-transcript hook", () => {
    const src = read(TILE);
    // `useGetExecutionSession(` — the full hook — must not appear as a CALL. (The import of
    // the tail hook contains the same prefix, so match the call form with the paren.)
    expect(
      src.includes("useGetExecutionSession("),
      `${TILE} calls useGetExecutionSession — that is the 10,000-part fetch. A tile wants ` +
        `the last text block; use useGetExecutionSessionTail.`,
    ).toBe(false);
    expect(src).toContain("useGetExecutionSessionTail(");
  });

  it("asks for a bounded tail, not 10000 parts", () => {
    const src = read(API);
    // The full-transcript hook still exists and still serves the 10,000-part payload — it is
    // the CHAT PANE's, and removing it would break the transcript view.
    expect(src).toContain("export function useGetExecutionSession(");
    expect(src).toContain("export function useGetExecutionSessionTail(");
    // The tail hook's default must be SMALL, because extractLastTextBlock walks backwards
    // from the end and stops at the first text part. Assert the number, not its spelling.
    const tail = src.slice(src.indexOf("export function useGetExecutionSessionTail("));
    const decl = tail.slice(0, tail.indexOf(") {"));
    const m = /limit = (\d+)/.exec(decl);
    expect(m, `the tail hook has no default limit:\n${decl}`).not.toBeNull();
    expect(Number(m![1]), "a tile's tail must be small — it reads one text block").toBeLessThanOrEqual(500);
  });

  it("gives the tail its OWN query key, so the page's burst cannot re-fetch it", () => {
    const src = read(API);
    expect(src).toContain("sessionTail: (id: string, limit: number)");
    // A distinct key from `session`: two payloads three orders of magnitude apart must not
    // share a cache entry, or invalidation of one re-fetches the other.
    expect(src).not.toContain("sessionTail: (id: string, limit: number) => [...executionKeys.all, \"session\", id]");
  });
});

describe("the execution page does not drag the full transcript along with its event burst", () => {
  it("does NOT invalidate the shared session key on every burst", () => {
    const src = read(PAGE);
    // The burst list is the argument to useDebouncedInvalidation. The session key must not
    // be one of them: it is the largest response in the app and it is re-fetched twice a
    // second for the length of a live run.
    const burst = src.slice(
      src.indexOf("useDebouncedInvalidation(["),
      src.indexOf("]);", src.indexOf("useDebouncedInvalidation([")),
    );
    expect(burst, "the burst must exist").not.toBe("");
    expect(
      burst.includes("executionKeys.session("),
      `the event burst still invalidates executionKeys.session() — that re-fetches the ` +
        `full 10,000-part transcript on every 500 ms burst:\n${burst}`,
    ).toBe(false);
    // The cheap keys stay, so the page still converges.
    expect(burst).toContain("executionKeys.detail(");
    expect(burst).toContain("executionKeys.todos(");
  });
});
