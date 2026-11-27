// tool-row-subject.test.ts — the GUI half of the CROSS-CLIENT tool-row contract.
//
// It reads internal/toolclass/testdata/row_subject_fixture.json VERBATIM — the same bytes
// internal/toolclass/row_subject_test.go asserts against — so a change to either implementation that is
// not mirrored in the other fails one of the two suites. That is the point: a transcript row is drawn by
// both clients from the same recorded part, and two answers would be a bug an operator cannot explain.
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { collapseWhitespace, isWebTool, rowSubject } from "@/lib/tool-row-subject";

interface FixtureCase {
  name: string;
  tool: string;
  input: string;
  want: string;
}
interface Fixture {
  cases: FixtureCase[];
}

const FIXTURE = resolve(
  process.cwd(),
  "../internal/toolclass/testdata/row_subject_fixture.json",
);

const fx: Fixture = JSON.parse(readFileSync(FIXTURE, "utf8"));

describe("rowSubject — the shared Go/TS tool-row contract", () => {
  it("has cases to assert (a vacuous fixture would make this contract meaningless)", () => {
    expect(fx.cases.length).toBeGreaterThan(0);
  });

  for (const c of fx.cases) {
    it(c.name, () => {
      expect(rowSubject(c.tool, c.input)).toBe(c.want);
    });
  }
});

describe("rowSubject — the agy argument spellings", () => {
  // The cases that motivate the file, spelled out as their OWN assertions so a regression names the
  // exact agy tool that lost its subject rather than only the fixture row.
  const agy = [
    { canonical: "bash", input: `{"CommandLine":"go build ./..."}`, want: "go build ./..." },
    { canonical: "write", input: `{"TargetFile":"/p/a.md"}`, want: "/p/a.md" },
    { canonical: "edit", input: `{"TargetFile":"/p/c.go"}`, want: "/p/c.go" },
    { canonical: "read", input: `{"AbsolutePath":"/p/e.go"}`, want: "/p/e.go" },
    { canonical: "grep", input: `{"query":"Ask"}`, want: "Ask" },
    { canonical: "webfetch", input: `{"Url":"https://x.example.com/y"}`, want: "https://x.example.com/y" },
    { canonical: "websearch", input: `{"query":"agy"}`, want: "agy" },
  ];
  for (const c of agy) {
    it(`${c.canonical} renders its subject`, () => {
      expect(rowSubject(c.canonical, c.input)).toBe(c.want);
      expect(rowSubject(c.canonical, c.input).trim()).not.toBe("");
    });
  }
});

describe("rowSubject — total, never invents a subject", () => {
  for (const bad of ["", "null", "  ", "{", "[]", "[1,2,3]", "42", "true", `{"unclosed":`]) {
    it(`yields "" for ${JSON.stringify(bad)}`, () => {
      expect(rowSubject("bash", bad)).toBe("");
    });
  }
  it("does not flatten an object-valued field into the row", () => {
    expect(rowSubject("write", `{"CodeContent":{"nested":true}}`)).toBe("");
  });
});

describe("isWebTool / collapseWhitespace", () => {
  it("labels the web tools and nothing else", () => {
    for (const n of ["webfetch", "web_fetch", "read_url_content", "websearch", "WebFetch"]) {
      expect(isWebTool(n)).toBe(true);
    }
    for (const n of ["bash", "read", "write", "", "grep"]) {
      expect(isWebTool(n)).toBe(false);
    }
  });

  it("keeps a heredoc command on one row", () => {
    const got = collapseWhitespace("cat > f <<'EOF'\nline one\nline two\nEOF");
    expect(got).toBe("cat > f <<'EOF' line one line two EOF");
    expect(got).not.toContain("\n");
  });
});
