// ask-tool-rows.test.ts — the GUI's durable tool-call rows (AC3's transcript half).
//
// WHY THIS EXISTS AT ALL. `MessageBubble` drew a message's prose, reasoning and ask card, and DROPPED its
// recorded tool calls: rows existed only while the SSE stream was live, so every call vanished the moment
// a turn finalized and the page was re-read. On an antigravity turn those rows ARE the record of the work
// (agy reports `run_command`/`view_file`/`read_url_content` as tool calls), so the defect emptied the very
// transcript this parity item is about. A reloaded agy conversation showed the model's reply with no
// evidence behind it, and AC3's mode-refused capture could not be produced.
//
// It also pins the CROSS-CLIENT subject: the same fixture bytes the Go test reads
// (internal/toolclass/testdata/row_subject_fixture.json) go through `rowSubject`, which is what ToolCard
// draws beside each name. An agy row that showed a truncated JSON object instead of its command, path or
// URL is the bug this contract prevents.
import { describe, expect, it } from "vitest";
import { durableToolRows, toolCallRendered, toolRowKey } from "@/lib/ask-tool-rows";
import { collapseWhitespace, rowSubject } from "@/lib/tool-row-subject";

/** A recorded agy message: the fixture's own tool calls and results, as the transcript page carries them. */
const AGY_CALLS = [
  {
    id: "tc-agy-1",
    functionName: "run_command",
    arguments: '{"CommandLine":"go test ./internal/tui/... -run Antigravity","Cwd":"/home/op/project"}',
  },
  {
    id: "tc-agy-2",
    functionName: "view_file",
    arguments: '{"AbsolutePath":"/home/op/project/README.md"}',
  },
  {
    id: "tc-agy-3",
    functionName: "read_url_content",
    arguments: '{"Url":"https://antigravity.google/download"}',
  },
  {
    id: "tc-agy-4",
    functionName: "write_file",
    arguments: '{"TargetFile":"/home/op/project/probe.txt","CodeContent":"ok\\n"}',
  },
];
const AGY_RESULTS = [
  { toolCallId: "tc-agy-1", output: "ok  internal/tui/modelpick  0.006s" },
  { toolCallId: "tc-agy-2", output: "# Orchicon" },
  { toolCallId: "tc-agy-3", output: "Download Antigravity (200)" },
  { toolCallId: "tc-agy-4", output: "REFUSED BY THE PLATFORM: write is not available in brainstorm mode", isError: true },
];

describe("durableToolRows — a recorded turn's tool calls become rows", () => {
  it("draws one row per agy call, in ledger order", () => {
    const rows = durableToolRows({ id: "a-agy-1", toolCalls: AGY_CALLS, toolResults: AGY_RESULTS });
    expect(rows.map((r) => r.toolName)).toEqual([
      "run_command",
      "view_file",
      "read_url_content",
      "write_file",
    ]);
  });

  it("pairs each call with ITS result, not merely the next one", () => {
    const rows = durableToolRows({ id: "a-agy-1", toolCalls: AGY_CALLS, toolResults: AGY_RESULTS });
    expect(rows[0].output).toBe("ok  internal/tui/modelpick  0.006s");
    expect(rows[2].output).toBe("Download Antigravity (200)");
    // THE REFUSED CALL keeps the platform's refusal text — the words AC3's mode-refused frame asserts.
    expect(rows[3].output).toContain("REFUSED BY THE PLATFORM");
  });

  it("gives every row a subject from agy's OWN argument keys", () => {
    const rows = durableToolRows({ id: "a-agy-1", toolCalls: AGY_CALLS, toolResults: AGY_RESULTS });
    const subjects = rows.map((r) => collapseWhitespace(rowSubject(r.toolName, r.input)));
    expect(subjects[0]).toBe("go test ./internal/tui/... -run Antigravity");
    expect(subjects[1]).toBe("/home/op/project/README.md");
    expect(subjects[2]).toBe("https://antigravity.google/download");
    expect(subjects[3]).toBe("/home/op/project/probe.txt");
  });

  it("draws no row for an ask_user call or a permission ledger row", () => {
    const rows = durableToolRows({
      id: "a-1",
      toolCalls: [
        { id: "c1", functionName: "ask_user", arguments: '{"question":"which?"}' },
        { id: "c2", functionName: "permission.deny", arguments: "" },
        { id: "c3", functionName: "run_command", arguments: '{"CommandLine":"ls"}' },
      ],
      toolResults: [],
    });
    expect(rows.map((r) => r.toolName)).toEqual(["run_command"]);
  });

  it("is empty — not an error — for a message with no calls", () => {
    expect(durableToolRows({ id: "a-1" })).toEqual([]);
    expect(durableToolRows(null)).toEqual([]);
  });

  it("keeps the output of an id-less call instead of dropping it", () => {
    const rows = durableToolRows({
      id: "a-1",
      toolCalls: [{ functionName: "run_command", arguments: '{"CommandLine":"pwd"}' }],
      toolResults: [{ output: "/home/op/project" }],
    });
    expect(rows).toHaveLength(1);
    expect(rows[0].output).toBe("/home/op/project");
  });

  it("carries the result's error flag, so a refusal is drawn in full", () => {
    // THE REFUSED CALL. `failed` is what lets ToolCard show the platform's refusal WITHOUT the operator
    // opening the row: for a refusal the text IS the message (why the tool did not run and what to
    // change), so collapsing it behind a `view` toggle and a byte count hides the only actionable part.
    // Mirrors ParsedTool.Failed in internal/tui/chat/grouping.go.
    const rows = durableToolRows({ id: "a-agy-1", toolCalls: AGY_CALLS, toolResults: AGY_RESULTS });
    const refused = rows.find((r) => r.toolName === "write_file");
    if (!refused) throw new Error("the refused call must still become a row");
    expect(refused.failed).toBe(true);
    // AND A NORMAL RESULT IS NOT FLAGGED, or every row would render as an error.
    for (const ok of rows.filter((r) => r.toolName !== "write_file")) {
      expect(ok.failed).toBe(false);
    }
  });

  it("flags an orphan result's error too, not only an id-paired one", () => {
    const rows = durableToolRows({
      id: "a-1",
      toolCalls: [{ functionName: "run_command", arguments: '{"CommandLine":"pwd"}' }],
      toolResults: [{ output: "denied", isError: true }],
    });
    expect(rows[0].failed).toBe(true);
  });

  it("keys an id-less row by position so two calls cannot collide", () => {
    const a = { id: "", toolName: "run_command", input: "", output: "", at: 0 };
    const b = { id: "", toolName: "run_command", input: "", output: "", at: 0 };
    expect(toolRowKey(a, 0)).not.toBe(toolRowKey(b, 1));
  });
});

describe("toolCallRendered — the two kinds that are not rows", () => {
  it("renders an ordinary call", () => {
    expect(toolCallRendered("run_command")).toBe(true);
    expect(toolCallRendered("read_url_content")).toBe(true);
  });
  it("refuses a card and a permission ledger row", () => {
    expect(toolCallRendered("ask_user")).toBe(false);
    expect(toolCallRendered("mcp_orchicon_ask_user")).toBe(false);
    expect(toolCallRendered("permission.user_PERMISSION_CHOICE_DENY")).toBe(false);
    expect(toolCallRendered("")).toBe(false);
  });
});
