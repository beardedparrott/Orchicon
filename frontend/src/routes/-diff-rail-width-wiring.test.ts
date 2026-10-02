// Per-page rail-width wiring on BOTH mounts.
//
// The rail's width is host-owned and persisted per page (the same discipline
// as open/tab/selectedPath). This is acceptance-critical and easy to regress
// silently: a single shared key would let the Ask page and an execution page
// overwrite each other's width, and a mount that forgot to pass containerRef
// would lose the container clamp (back to squeezing the chat column). Repo
// idiom for route-level contracts is a source assertion (no jsdom here).
import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

const ask = fs.readFileSync(path.join(__dirname, "ask-orchicon.tsx"), "utf8");
const exec = fs.readFileSync(path.join(__dirname, "executions_.$id.tsx"), "utf8");

describe("rail width is persisted per page under distinct keys", () => {
  it("the Ask page uses its own ask-orchicon:railWidth key", () => {
    expect(ask).toContain('"ask-orchicon:railWidth"');
  });

  it("the execution page namespaces its key by execution id", () => {
    expect(exec).toContain("`execution:${id}:railWidth`");
  });

  it("the two keys cannot collide", () => {
    // The execution key is a template carrying the id; the Ask key is a literal.
    // A shared literal key would make one page clobber the other.
    expect(ask).not.toContain("execution:${id}:railWidth");
    expect(exec).not.toContain("ask-orchicon:railWidth");
  });

  it("both mounts seed the width from the shared default constant", () => {
    for (const src of [ask, exec]) {
      expect(src).toContain("RAIL_DEFAULT_WIDTH");
    }
  });
});

describe("both mounts give the rail its container clamp + width", () => {
  it("the Ask page row is the rail's container and drives the width", () => {
    expect(ask).toMatch(/<div ref=\{diffRowRef\}[^>]*>/);
    expect(ask).toContain("containerRef={diffRowRef}");
    expect(ask).toContain("width={railWidth}");
    expect(ask).toContain("onWidthChange={setRailWidth}");
  });

  it("the execution page row is the rail's container and drives the width", () => {
    expect(exec).toMatch(/<div ref=\{diffRowRef\}[^>]*>/);
    expect(exec).toContain("containerRef={diffRowRef}");
    expect(exec).toContain("width={railWidth}");
    expect(exec).toContain("onWidthChange={setRailWidth}");
  });

  it("clamps against the row the rail is the FIRST child of on both mounts", () => {
    // The clamp reserves MIN_CHAT_WIDTH out of THIS node's width, so the row
    // must be the element that holds rail + chat side by side.
    for (const src of [ask, exec]) {
      const refIdx = src.indexOf("ref={diffRowRef}");
      const railIdx = src.indexOf("<DiffSidebar", refIdx);
      expect(refIdx).toBeGreaterThan(-1);
      expect(railIdx).toBeGreaterThan(refIdx);
    }
  });
});
