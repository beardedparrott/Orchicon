import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

describe("RuntimeImageFailureAlert", () => {
  it("renders amber alert with icon, reason, collapsible log and copy", () => {
    const src = fs.readFileSync(path.join(__dirname, "RuntimeImageFailureAlert.tsx"), "utf8");
    expect(src).toContain("border-amber");
    expect(src).toContain("bg-amber-50");
    expect(src).toContain("AlertTriangle");
    expect(src).toContain("Copy");
    expect(src).toContain("Show log");
    expect(src).toContain('role="alert"');
    expect(src).not.toContain("text-destructive");
    expect(src).not.toContain("bg-red");
  });
});
