// ask-tool-summary.test.ts — THE GUI'S COUNTER IS THE TUI'S COUNTER.
//
// AC2 says the line "reports work with the shared summarizer's output for the same ledger JSON,
// identical to the TUI's string". That is not a hope: the SAME fixture file
// (internal/toolclass/testdata/rollup_fixture.json) is enumerated by internal/toolclass/summarize_test.go
// and by this suite, so the two ports cannot drift. The fixture's own _comment names this consumer.
//
// It is the first cross-tree read from the frontend suite; the fallback, if a runner ever sandboxes
// frontend/, is a frontend-side copy plus a Go byte-equality test — the ask-verbs.json pattern.
import fs from "node:fs";
import path from "node:path";

import { describe, expect, it } from "vitest";

import {
  TOOL_SUMMARY_WINDOW_MS,
  classifyTool,
  summarizeToolCalls,
  toolCallsFromMessages,
  toolCountPhrase,
  type ToolCallStamp,
} from "@/lib/ask-tool-summary";

const FIXTURE = path.resolve(
  __dirname,
  "../../../internal/toolclass/testdata/rollup_fixture.json",
);

type FixtureCase = {
  name: string;
  ledger: { function_name?: string; issued_at_unix_ms?: number }[];
  now: number;
  window: number;
  want: string;
};

const fixture: { cases: FixtureCase[] } = JSON.parse(
  fs.readFileSync(FIXTURE, "utf8"),
);

function stampsOf(c: FixtureCase): ToolCallStamp[] {
  return c.ledger.map((row) => ({
    toolName: row.function_name ?? "",
    atMs: row.issued_at_unix_ms ?? 0,
  }));
}

describe("ask-tool-summary — the shared cross-client counter", () => {
  it("reads the Go-side fixture (it exists, and it has cases)", () => {
    // Guards the guard: a moved fixture must fail loudly here rather than pass vacuously.
    expect(fixture.cases.length).toBeGreaterThan(10);
    expect(TOOL_SUMMARY_WINDOW_MS).toBe(30_000);
  });

  it("AC2 — renders the EXACT string toolclass.Summarize renders, for every shared case", () => {
    for (const c of fixture.cases) {
      const got = summarizeToolCalls(stampsOf(c), c.now, c.window);
      expect(got, `${c.name} (${FIXTURE})`).toBe(c.want);
    }
  });

  it("AC6 — zero tool calls renders no counters at all, never '0 modifies'", () => {
    const out = summarizeToolCalls([], 1_000, TOOL_SUMMARY_WINDOW_MS);
    expect(out).toBe("");
    expect(out).not.toContain("0 ");
    expect(toolCountPhrase([], 1_000, TOOL_SUMMARY_WINDOW_MS)).toBe("");
  });

  it("AC6 — a ledger of only excluded tools is also the empty string", () => {
    const excluded: ToolCallStamp[] = [
      { toolName: "ask_user", atMs: 60_000 },
      { toolName: "todowrite", atMs: 60_000 },
      { toolName: "mcp__github__create_issue", atMs: 60_000 },
      { toolName: "permission.allow_once", atMs: 60_000 },
    ];
    expect(summarizeToolCalls(excluded, 61_000, TOOL_SUMMARY_WINDOW_MS)).toBe("");
    // A real read tool in the same window is counted, so the emptiness above is exclusion, not
    // a broken window.
    expect(
      summarizeToolCalls(
        [...excluded, { toolName: "todoread", atMs: 60_000 }],
        61_000,
        TOOL_SUMMARY_WINDOW_MS,
      ),
    ).toBe("1 read · last 1s");
  });

  it("D4 — a bigint stamp is COERCED, not compared (protoInt64 is a BigInt at runtime)", () => {
    const messages = [
      { toolCalls: [{ functionName: "write", issuedAtUnixMs: BigInt(61_000) }] },
    ];
    expect(toolCallsFromMessages(messages)).toEqual([
      { toolName: "write", atMs: 61_000 },
    ]);
    // Without Number() this throws `Cannot mix BigInt and other types` rather than rendering "".
    expect(
      summarizeToolCalls(toolCallsFromMessages(messages), 61_000, TOOL_SUMMARY_WINDOW_MS),
    ).toBe("1 modify · last 0s");
  });

  it("counts a ROLLING window, not a total", () => {
    const calls: ToolCallStamp[] = [
      { toolName: "read", atMs: 25_000 }, // 40s before `now` — out
      { toolName: "read", atMs: 64_000 }, // 1s before `now` — in
    ];
    expect(summarizeToolCalls(calls, 65_000, TOOL_SUMMARY_WINDOW_MS)).toBe(
      "1 read · last 1s",
    );
  });

  it("agrees with the Go vocabulary over the whole name table", () => {
    const cases: [string, string][] = [
      ["write", "modify"],
      ["edit", "modify"],
      ["batch_write", "modify"],
      ["orchicon_write", "modify"],
      ["WRITE", "modify"],
      ["  edit  ", "modify"],
      ["read", "read"],
      ["batch_read", "read"],
      ["grep", "read"],
      ["batch_grep", "read"],
      ["list", "read"],
      ["glob", "read"],
      ["todoread", "read"],
      ["bash", "bash"],
      ["shell", "bash"],
      ["orchicon_bash", "bash"],
      ["permission.allow_once", "ignore"],
      ["permission.deny", "ignore"],
      ["ask_user", "ignore"],
      ["askuser", "ignore"],
      ["ask_user_question", "ignore"],
      ["orchicon_ask_user", "ignore"],
      ["mcp__github__create_issue", "ignore"],
      ["todowrite", "ignore"],
      ["orchicon_list_projects", "ignore"],
      ["", "ignore"],
      ["nonsense", "ignore"],
    ];
    for (const [name, want] of cases) {
      expect(classifyTool(name), name).toBe(want);
    }
  });
});
