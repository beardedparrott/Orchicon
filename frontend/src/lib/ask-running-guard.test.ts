import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// THE RAIL'S RUNNING ROW READS THE UNION, NOT THE POLLED FIELD ALONE.
//
// The operator: "I have noticed the 'running' status on the conversation rail list doesn't always show up on
// active running conversations." The row rendered `conv.turnInFlight` by itself, so a conversation THIS
// client was streaming still read as idle until the next list poll — while the pane beside it already unioned
// the local stream slot. The fix routes every row through lib/ask-running.ts's `runningConvIds`.
//
// WHY A SOURCE ASSERTION: the DECISION is a pure function, tested in ask-running.test.ts. But the DEFECT was
// a WIRING OMISSION — the route's row passed `conv.turnInFlight ?? false` and nothing about that expression
// is wrong in isolation; only its combination with the client's own live stream state was. The route
// (frontend/src/routes/ask-orchicon.tsx) cannot be rendered by this repo's test setup (providers + sockets +
// 4,000 lines, as -ask-orchicon.activity-line.guard.test.ts's header records), so the wiring is pinned here,
// in the same style as that guard: assert the route passes the union to the row and no longer reads the
// polled field directly.

const SRC = path.join(__dirname, "..");
const ROUTE = "routes/ask-orchicon.tsx";

function read(rel: string): string {
  return fs.readFileSync(path.join(SRC, rel), "utf8");
}

describe("the Ask rail row reads the running union (lib/ask-running)", () => {
  it("imports runningConvIds", () => {
    const src = read(ROUTE);
    expect(src).toContain('from "@/lib/ask-running"');
    expect(src).toContain("runningConvIds(");
  });

  it("no longer keys a row's isRunning off the polled turnInFlight field alone", () => {
    const src = read(ROUTE);
    expect(src).not.toContain("isRunning={conv.turnInFlight ?? false}");
  });

  it("passes the union set to every row-rendering list", () => {
    const src = read(ROUTE);
    // The folded, uncategorized and mobile lists all read the SAME set, so the decision cannot drift.
    const sites = src.match(/isRunning=\{runningIds\.has\(convId\)\}/g) ?? [];
    expect(sites.length).toBeGreaterThanOrEqual(2);
    // And each list receives the set (FolderItem + UncategorizedDropZone, desktop + mobile).
    const passes = src.match(/runningIds=\{runningIds\}/g) ?? [];
    expect(passes.length).toBeGreaterThanOrEqual(4);
  });

  it("invalidates the conversations list on a turn ack, so a send is reflected at once", () => {
    const src = read(ROUTE);
    // The turnStarted arm is where the ack arrives; the invalidate must be inside it — i.e. between the
    // turnStarted case and the NEXT case, so a later arm's invalidate cannot satisfy this.
    const idx = src.indexOf('chunk.event.case === "turnStarted"');
    expect(idx).toBeGreaterThan(-1);
    const next = src.indexOf("chunk.event.case", idx + 1);
    const arm = src.slice(idx, next > idx ? next : idx + 2000);
    expect(arm).toContain("invalidateQueries({ queryKey: askKeys.conversations })");
  });
});
