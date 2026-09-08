import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  createDebouncedInvalidation,
  executionStreamEnabled,
} from "./debouncedInvalidation";

describe("createDebouncedInvalidation", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("collapses a burst of N events into one invalidation after the quiet period", () => {
    const invalidate = vi.fn();
    const d = createDebouncedInvalidation(invalidate, 500);
    for (let i = 0; i < 50; i++) d.schedule();
    expect(invalidate).not.toHaveBeenCalled();
    vi.advanceTimersByTime(499);
    expect(invalidate).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(invalidate).toHaveBeenCalledTimes(1);
  });

  it("resets the trailing timer on each event (quiet period from the last)", () => {
    const invalidate = vi.fn();
    const d = createDebouncedInvalidation(invalidate, 500);
    d.schedule();
    vi.advanceTimersByTime(300);
    d.schedule(); // resets the timer
    vi.advanceTimersByTime(300);
    expect(invalidate).not.toHaveBeenCalled();
    vi.advanceTimersByTime(200);
    expect(invalidate).toHaveBeenCalledTimes(1);
  });

  it("flush fires a pending invalidation immediately and is a no-op when idle", () => {
    const invalidate = vi.fn();
    const d = createDebouncedInvalidation(invalidate, 500);
    d.schedule();
    d.flush();
    expect(invalidate).toHaveBeenCalledTimes(1);
    d.flush();
    expect(invalidate).toHaveBeenCalledTimes(1);
  });

  it("cancel drops a pending invalidation without firing", () => {
    const invalidate = vi.fn();
    const d = createDebouncedInvalidation(invalidate, 500);
    d.schedule();
    d.cancel();
    vi.advanceTimersByTime(1000);
    expect(invalidate).not.toHaveBeenCalled();
  });
});

describe("executionStreamEnabled", () => {
  it("opens a stream only for a non-terminal execution with an id", () => {
    expect(executionStreamEnabled("e1", false)).toBe(true);
    expect(executionStreamEnabled("e1", true)).toBe(false);
    expect(executionStreamEnabled("", false)).toBe(false);
    expect(executionStreamEnabled("", true)).toBe(false);
  });
});
