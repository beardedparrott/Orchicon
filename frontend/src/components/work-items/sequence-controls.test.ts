// Unit tests for the run-control gating. The buttons are a pure function of the item's
// server-reported status + whether it has children (a parent is a sequence run; a leaf runs its
// own bound workflow). The server does the real validation — these tests pin the display gating.
//
// THESE ASSERTIONS CHANGED WITH THE SEMANTICS, DELIBERATELY.
//
// The old expectations had a leaf offering START and RESUME where both were the same call, and
// STOP on a leaf — which ABORTS the run and (before this change) deleted the branch. Pinning that
// arrangement was pinning the bug. What the controls must express now:
//
//   PAUSE   offered only while something is in flight, and it keeps the work
//   RESUME  offered when there is failed/cancelled work to continue
//   RESTART the old START, destructive, offered whenever nothing is in flight
//   STOP    a sequence-parent gesture; on a leaf, Pause replaces it

import { describe, expect, it } from "vitest";

import {
  WorkItemStatus,
} from "@/api/gen/orchicon/api/v1/work_item_pb";
import { SequenceAction } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import { availableActions } from "./sequence-controls";

const S = WorkItemStatus;

describe("runControlsFor (parent — has children, a sequence run)", () => {
  it("offers PAUSE + STOP while running, never RESTART", () => {
    expect(availableActions(S.RUNNING, true)).toEqual([
      SequenceAction.PAUSE,
      SequenceAction.STOP,
    ]);
  });
  it("offers RESTART + STOP when parked (pending), no RESUME", () => {
    // A PARKED chain is not a FAILED one: there is no halted work to continue, so RESUME is
    // withheld — offering it would be exactly the lie the leaf Resume made before. RESTART is
    // the old START, and STOP keeps a parked-but-scheduled chain from advancing.
    expect(availableActions(S.PENDING, true)).toEqual([
      SequenceAction.RESTART,
      SequenceAction.STOP,
    ]);
  });
  it("offers RESUME + RESTART + STOP when failed (halted chain)", () => {
    expect(availableActions(S.FAILED, true)).toEqual([
      SequenceAction.RESUME,
      SequenceAction.RESTART,
      SequenceAction.STOP,
    ]);
  });
  it("offers only RESTART when succeeded", () => {
    // A finished chain can be re-fired, and nothing else means anything on it.
    expect(availableActions(S.SUCCEEDED, true)).toEqual([SequenceAction.RESTART]);
  });
});

describe("runControlsFor (leaf — no children, runs its own workflow)", () => {
  it("offers PAUSE while running, and no RESTART to collide with it", () => {
    expect(availableActions(S.RUNNING, false)).toEqual([SequenceAction.PAUSE]);
  });
  it("offers RESUME + RESTART when failed — continue, or start over", () => {
    // THE PAIR THAT DID NOT EXIST BEFORE: the same status now offers both, and they differ.
    expect(availableActions(S.FAILED, false)).toEqual([
      SequenceAction.RESUME,
      SequenceAction.RESTART,
    ]);
  });
  it("offers RESUME + RESTART when cancelled — abort no longer destroys the branch", () => {
    expect(availableActions(S.CANCELLED, false)).toEqual([
      SequenceAction.RESUME,
      SequenceAction.RESTART,
    ]);
  });
  it("offers RESTART when pending", () => {
    expect(availableActions(S.PENDING, false)).toEqual([SequenceAction.RESTART]);
  });
  it("offers RESTART when succeeded", () => {
    expect(availableActions(S.SUCCEEDED, false)).toEqual([SequenceAction.RESTART]);
  });
  it("never offers STOP on a leaf — Pause is the non-destructive stop", () => {
    for (const status of [S.PENDING, S.RUNNING, S.FAILED, S.CANCELLED, S.SUCCEEDED]) {
      expect(availableActions(status, false)).not.toContain(SequenceAction.STOP);
    }
  });
});
