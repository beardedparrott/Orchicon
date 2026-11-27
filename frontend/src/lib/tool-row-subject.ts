// tool-row-subject.ts — the GUI's half of the ONE tool-row subject (the Go half is
// internal/toolclass/row_subject.go).
//
// WHY THIS IS A MIRROR AND NOT ITS OWN FUNCTION. A tool row has room for one line beside the tool's
// name, and the GUI and the TUI draw that same transcript row from the same recorded parts. Two
// implementations would be two answers to "what was this call about", and the drifted one is a bug an
// operator sees and cannot explain.
//
// THE AGY KEYS ARE THE REASON IT EXISTS NOW. internal/antigravity canonicalises `run_command` to `bash`
// and `read_url_content` to `webfetch`, but the recorded input keeps agy's OWN argument spellings
// (`CommandLine`, `TargetFile`, `Url`, `query`). A renderer that knew only opencode's `command`/
// `file_path` drew an empty subject for every antigravity call — the row said "bash" and nothing else.
//
// PINNED CROSS-CLIENT: internal/toolclass/testdata/row_subject_fixture.json is read VERBATIM by the Go
// test AND by this file's Vitest suite, so the two cannot drift.

/** The ordered argument keys that carry a tool call's SUBJECT, per subject kind. Ordered and
 *  cross-vendor on purpose: the first non-empty value wins, so a call carrying both opencode's
 *  `command` and agy's `CommandLine` renders the same line either way. */
const COMMAND_KEYS = ["CommandLine", "command_line", "command", "Command", "cmd", "script"];
const PATH_KEYS = [
  "Path",
  "path",
  "file_path",
  "AbsolutePath",
  "absolute_path",
  "TargetFile",
  "target_file",
  "SearchPath",
  "search_path",
  "filePath",
  "notebook_path",
  "DirectoryPath",
  "directory_path",
];
const URL_KEYS = ["Url", "url", "URL", "uri", "href"];
const QUERY_KEYS = ["query", "Query", "search_query", "searchQuery", "pattern", "Pattern", "q"];

/** The modifier/read/bash sets, mirroring internal/toolclass (which the activity line already pins). */
const MODIFY_TOOLS = new Set([
  "write",
  "edit",
  "batch_write",
  "write_file",
  "write_to_file",
  "create_file",
  "edit_file",
  "replace_file_content",
  "multi_replace_file_content",
  "apply_patch",
]);
const READ_TOOLS = new Set([
  "read",
  "batch_read",
  "grep",
  "batch_grep",
  "list",
  "glob",
  "todoread",
  "read_file",
  "view_file",
  "open_file",
  "grep_search",
  "list_dir",
  "list_directory",
  "find_by_name",
]);
const BASH_TOOLS = new Set(["bash", "shell", "run_command"]);

type SubjectClass = "modify" | "read" | "bash" | "other";

/** classifyForSubject mirrors toolclass.Classify closely enough to pick the subject ORDER. It strips the
 *  `orchicon_`/`mcp_orchicon_` prefixes the Go side strips, because an MCP tool's subject is still its
 *  first scalar. */
function classifyForSubject(toolName: string): SubjectClass {
  let n = (toolName ?? "").toLowerCase().trim();
  if (n.startsWith("permission.")) return "other";
  n = n.replace(/^mcp_orchicon_/, "").replace(/^orchicon_/, "");
  if (n === "") return "other";
  if (MODIFY_TOOLS.has(n)) return "modify";
  if (READ_TOOLS.has(n)) return "read";
  if (BASH_TOOLS.has(n)) return "bash";
  return "other";
}

/** asString renders a decoded JSON value as the string a row can show. Objects and arrays are NOT
 *  flattened: a row that printed a whole file body would defeat the point of a one-line subject. */
function asString(v: unknown): string {
  if (typeof v === "string") return v;
  if (typeof v === "number") return Number.isInteger(v) ? String(v) : String(v);
  if (typeof v === "boolean") return v ? "true" : "false";
  return "";
}

function firstOf(obj: Record<string, unknown>, keys: readonly string[]): string {
  for (const k of keys) {
    if (k in obj) {
      const s = asString(obj[k]);
      if (s.trim() !== "") return s;
    }
  }
  return "";
}

/** decodeInput tolerates the two shapes this platform records: a JSON object (the usual case) and a
 *  bare JSON string (an adapter that reported its whole input as text). */
function decodeInput(inputJSON: string): Record<string, unknown> | null {
  const s = (inputJSON ?? "").trim();
  if (s === "" || s === "null") return null;
  try {
    const parsed: unknown = JSON.parse(s);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>;
    }
    if (typeof parsed === "string") {
      return { [COMMAND_KEYS[0]]: parsed };
    }
  } catch {
    /* not JSON: the Go side yields "" for the same input */
  }
  return null;
}

/**
 * rowSubject returns the one-line subject to draw beside a tool call's name, or "" when the input
 * carries nothing a human should read. It is PURE and TOTAL: a nil/empty/malformed input yields "".
 */
export function rowSubject(toolName: string, inputJSON: string): string {
  const obj = decodeInput(inputJSON);
  if (!obj) return "";
  switch (classifyForSubject(toolName)) {
    case "bash":
      return firstOf(obj, COMMAND_KEYS);
    case "modify":
    case "read": {
      // A read/grep with no path names its pattern instead: `grep TODO` is the subject, `grep` is not.
      const p = firstOf(obj, PATH_KEYS);
      return p !== "" ? p : firstOf(obj, QUERY_KEYS);
    }
    default: {
      for (const keys of [URL_KEYS, QUERY_KEYS, COMMAND_KEYS, PATH_KEYS]) {
        const v = firstOf(obj, keys);
        if (v !== "") return v;
      }
      // A STABLE key order, so an unknown tool's subject does not change between two renders.
      for (const k of Object.keys(obj).sort()) {
        const s = asString(obj[k]);
        if (s.trim() !== "") return s;
      }
      return "";
    }
  }
}

/** isWebTool mirrors toolclass.IsWebTool, so a renderer can label the subject. */
export function isWebTool(toolName: string): boolean {
  const n = (toolName ?? "").toLowerCase().trim();
  return (
    n === "webfetch" ||
    n === "web_fetch" ||
    n === "read_url_content" ||
    n === "websearch" ||
    n === "web_search" ||
    n === "search_web"
  );
}

/** collapseWhitespace flattens a multi-line subject (a heredoc command) onto one row. */
export function collapseWhitespace(s: string): string {
  return (s ?? "").split(/\s+/).filter(Boolean).join(" ");
}
