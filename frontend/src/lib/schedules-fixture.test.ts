// The CROSS-CLIENT parity harness for the Schedules Upcoming membership.
//
// The bug this guards: the TUI's Upcoming lens read only `status = SCHEDULED`, while the GUI's
// Upcoming unions that with the PENDING children of an active sequence parent
// (`queuedSequenceChildren`, schedules-model.ts). Same tenant, opposite answers — the TUI showed
// "nothing scheduled here".
//
// `schedules-fixture.json` is the ONE fixture both predicates are judged against: this file runs
// the GUI's `queuedSequenceChildren` over it, and
// internal/tui/screens/execution/schedules_queued_test.go runs the TUI's Go mirror over the SAME
// file. A drift between the two implementations fails a test in BOTH languages.
import fs from "node:fs";
import path from "node:path";

import type { JsonValue } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import {
  WorkItem,
  WorkItemStatus,
} from "@/api/gen/orchicon/api/v1/work_item_pb";
import { queuedSequenceChildren } from "@/lib/schedules-model";

type Fixture = {
  items: JsonValue[];
  wantQueued: string[];
  wantUpcoming: string[];
};

const fixture: Fixture = JSON.parse(
  fs.readFileSync(path.join(__dirname, "schedules-fixture.json"), "utf8"),
);

const items = fixture.items.map((raw) => new WorkItem().fromJson(raw));

describe("schedules-fixture — cross-client Upcoming membership", () => {
  it("derives exactly the queued children the fixture names, in CHAIN order", () => {
    expect(queuedSequenceChildren(items).map((i) => i.id)).toEqual(
      fixture.wantQueued,
    );
  });

  it("unions the queued children with SCHEDULED — the set the TUI must show", () => {
    const queued = queuedSequenceChildren(items).map((i) => i.id);
    const scheduled = items
      .filter((i) => i.status === WorkItemStatus.SCHEDULED)
      .map((i) => i.id);
    expect([...queued, ...scheduled]).toEqual(fixture.wantUpcoming);
  });

  it("excludes a pending child whose parent is not an active sequence parent", () => {
    const queued = queuedSequenceChildren(items).map((i) => i.id);
    expect(queued).not.toContain("wi-done-child");
    expect(queued).not.toContain("wi-loose-pending");
    expect(queued).not.toContain("wi-seq-1");
  });
});
