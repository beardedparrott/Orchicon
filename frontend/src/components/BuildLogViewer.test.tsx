import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

describe("BuildLogViewer", () => {
  it("is multiline vertically scrollable with auto-scroll and copy", () => {
    const src = fs.readFileSync(path.join(__dirname, "BuildLogViewer.tsx"), "utf8");
    expect(src).toContain("overflow-y-auto");
    expect(src).toContain("whitespace-pre-wrap");
    expect(src).toContain("scrollTop = el.scrollHeight");
    expect(src).toContain("Copy");
    expect(src).not.toContain("overflow-x-auto");
  });
});
