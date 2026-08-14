// Unit tests for the sequence-control action gating. The buttons are a
// pure function of the item's server-reported status + whether it has
// children (a parent is a sequence run; a leaf runs its own bound
// workflow). The server does the real validation — these tests pin the
// frontend's display gating so parent/leaf semantics don't regress.

import { describe, expect, it } from "vitest";

import {
  WorkItemStatus,
} from "@/api/gen/orchicon/api/v1/work_item_pb";
import { SequenceAction } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import { availableActions } from "./sequence-controls";

const S = WorkItemStatus;

describe("availableActions (parent — has children, a sequence run)", () => {
  it("offers STOP while running and nothing else", () => {
    expect(availableActions(S.RUNNING, true)).toEqual([SequenceAction.STOP]);
  });
  it("offers START + RESUME when parked (pending)", () => {
    expect(availableActions(S.PENDING, true)).toEqual([
      SequenceAction.START,
      SequenceAction.RESUME,
    ]);
  });
  it("offers START + RESUME + STOP when failed (halted chain)", () => {
    expect(availableActions(S.FAILED, true)).toEqual([
      SequenceAction.START,
      SequenceAction.RESUME,
      SequenceAction.STOP,
    ]);
  });
  it("offers nothing when succeeded", () => {
    expect(availableActions(S.SUCCEEDED, true)).toEqual([]);
  });
});

describe("availableActions (leaf — no children, runs its own workflow)", () => {
  it("offers START while pending", () => {
    expect(availableActions(S.PENDING, false)).toEqual([
      SequenceAction.START,
    ]);
  });
  it("offers START + STOP while running", () => {
    // A running leaf must always be stoppable; START is hidden (already on).
    expect(availableActions(S.RUNNING, false)).toEqual([SequenceAction.STOP]);
  });
  it("offers START + RESUME + STOP when failed", () => {
    expect(availableActions(S.FAILED, false)).toEqual([
      SequenceAction.START,
      SequenceAction.RESUME,
      SequenceAction.STOP,
    ]);
  });
  it("offers START + RESUME when cancelled", () => {
    expect(availableActions(S.CANCELLED, false)).toEqual([
      SequenceAction.START,
      SequenceAction.RESUME,
    ]);
  });
  it("offers nothing when succeeded", () => {
    expect(availableActions(S.SUCCEEDED, false)).toEqual([]);
  });
});
