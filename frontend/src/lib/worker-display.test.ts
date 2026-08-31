import { describe, it, expect } from "vitest";

import { WorkerVersionStatus } from "@/api/gen/orchicon/api/v1/worker_pb";
import { formatModelRef, versionStatusLabel, versionStatusTone } from "./worker-display";

describe("formatModelRef", () => {
  it("returns verbatim provider/id", () => {
    expect(formatModelRef("anthropic/claude-sonnet-4")).toBe("anthropic/claude-sonnet-4");
    expect(formatModelRef("openai/gpt-4o")).toBe("openai/gpt-4o");
  });
  it("trims whitespace", () => {
    expect(formatModelRef("  anthropic/claude-sonnet-4  ")).toBe("anthropic/claude-sonnet-4");
  });
  it("returns em dash for empty", () => {
    expect(formatModelRef("")).toBe("\u2014");
    expect(formatModelRef("   ")).toBe("\u2014");
  });
});

describe("versionStatusLabel", () => {
  it("maps each status", () => {
    expect(versionStatusLabel(WorkerVersionStatus.DRAFT)).toBe("draft");
    expect(versionStatusLabel(WorkerVersionStatus.PUBLISHED)).toBe("published");
    expect(versionStatusLabel(WorkerVersionStatus.DEPRECATED)).toBe("deprecated");
    expect(versionStatusLabel(WorkerVersionStatus.UNSPECIFIED)).toBe("unknown");
  });
});

describe("versionStatusTone", () => {
  it("returns distinct tones", () => {
    const draft = versionStatusTone(WorkerVersionStatus.DRAFT);
    const published = versionStatusTone(WorkerVersionStatus.PUBLISHED);
    const deprecated = versionStatusTone(WorkerVersionStatus.DEPRECATED);
    expect(draft).toContain("blue");
    expect(published).toContain("green");
    expect(deprecated).toContain("amber");
    expect(draft).not.toBe(published);
  });
});
