import { describe, expect, it } from "vitest";
import { parseWorkerSummary } from "./workerSummary";
import {
  SUMMARY_SUCCESS_OUTPUT,
  SUMMARY_SUCCESS_EMPTY_CONVERSATION,
  NO_SUMMARY_OUTPUT,
  SUMMARY_FAILURE_OUTPUT,
} from "./workerSummary.fixtures";

describe("parseWorkerSummary", () => {
  it("parses a success summary with FACTS LEARNED lines (01M1WB5TP9RCF1740MT93HJKQS shape)", () => {
    const s = parseWorkerSummary(SUMMARY_SUCCESS_OUTPUT);
    expect(s.present).toBe(true);
    expect(s.status).toBe("success");
    expect(s.rawStatus).toBe("success");
    expect(s.text).toBe("DEDICATED Ask session runtime decision");
    expect(s.factsLearned).toHaveLength(2);
    expect(s.factsLearned[0]).toContain("runtime container's supervisor");
    expect(s.factsLearned[1]).toContain("storedOutput only shows");
  });

  it("parses a success summary in the empty-conversation native-bridge case", () => {
    const s = parseWorkerSummary(SUMMARY_SUCCESS_EMPTY_CONVERSATION);
    expect(s.present).toBe(true);
    expect(s.status).toBe("success");
    expect(s.text).toBe("DEDICATED Ask session runtime decision");
    expect(s.factsLearned).toHaveLength(1);
  });

  it("returns present:false when Output has no summary marker", () => {
    const s = parseWorkerSummary(NO_SUMMARY_OUTPUT);
    expect(s.present).toBe(false);
    expect(s.status).toBe("unknown");
    expect(s.text).toBe("");
    expect(s.factsLearned).toEqual([]);
  });

  it("returns present:false for empty/undefined output", () => {
    expect(parseWorkerSummary(undefined).present).toBe(false);
    expect(parseWorkerSummary(null).present).toBe(false);
    expect(parseWorkerSummary("").present).toBe(false);
  });

  it("normalizes failure variant spellings", () => {
    const s = parseWorkerSummary(SUMMARY_FAILURE_OUTPUT);
    expect(s.present).toBe(true);
    expect(s.status).toBe("failure");
    expect(s.text).toBe("Found 3 bugs in the implementation.");
  });

  it("normalizes common success/failure variant words", () => {
    expect(parseWorkerSummary("ORCHICON WORKER SUMMARY: succeeded — done").status).toBe("success");
    expect(parseWorkerSummary("ORCHICON WORKER SUMMARY: failed — nope").status).toBe("failure");
    expect(parseWorkerSummary("ORCHICON WORKER SUMMARY: error — boom").status).toBe("failure");
  });

  it("treats unknown status words as unknown but still surfaces the text", () => {
    const s = parseWorkerSummary("ORCHICON WORKER SUMMARY: maybe — unclear");
    expect(s.present).toBe(true);
    expect(s.status).toBe("unknown");
    expect(s.text).toBe("unclear");
  });

  it("handles a marker with no trailing text", () => {
    const s = parseWorkerSummary("ORCHICON WORKER SUMMARY: success");
    expect(s.present).toBe(true);
    expect(s.status).toBe("success");
    expect(s.text).toBe("");
  });
});
