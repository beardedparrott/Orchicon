// -runtime-image-local-mode.test.tsx
//
// THE BUG THIS PINS. `RuntimeImageSelect` is already mode-aware: given mode="local" it renders
// "No image — a local run executes on the host, not in a container" and offers nothing to choose,
// because a local run has no container and therefore no image.
//
// But that behaviour only happens if the CALLER PASSES `mode`. The regular work-item forms do. The
// recurring forms did not — they passed only value/onChange — so choosing "local" on an automation
// still showed an image dropdown, and the form could submit an image for a run that will never create
// a container. The component's own doc comment records exactly this class of report: "I set it to local
// and it set it to 'default (base image)'".
//
// So there are two halves to assert: the component's local rendering, and the recurring forms actually
// wiring `mode` into it.

import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import fs from "node:fs";
import path from "node:path";
import { RuntimeImageSelect } from "./RuntimeImageSelect";

const NEW_FORM = path.resolve(__dirname, "../routes/recurring-items_.new.tsx");
const EDIT_FORM = path.resolve(__dirname, "../routes/recurring-items_.$id.tsx");
const read = (p: string) => fs.readFileSync(p, "utf8");

// The component queries the runtime-image catalogue, so it needs a client to render at all.
const withClient = (el: React.ReactElement) => {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, networkMode: "always" } },
  });
  return createElement(QueryClientProvider, { client: qc }, el);
};

describe("RuntimeImageSelect in local mode", () => {
  it("states that a local run has no image, and offers nothing to pick", () => {
    const html = renderToStaticMarkup(
      withClient(
        createElement(RuntimeImageSelect, { value: "", onChange: () => {}, mode: "local" }),
      ),
    );
    expect(html).toContain("No image — a local run executes on the host, not in a container");
    // No dropdown at all: there is nothing to choose.
    expect(html).not.toContain("<select");
    // And it says how to get one back.
    expect(html).toContain("Switch the execution mode to");
  });

  // The runtime-mode branch is a full react-query-backed <select>, whose server render needs the
  // hydrated client that renderToStaticMarkup cannot give it. Asserted from the source instead: the
  // local branch is the one that RETURNS EARLY, so the presence of that guard is what keeps runtime
  // mode on the picker path.
  it("only short-circuits when the mode is local", () => {
    const src = fs.readFileSync(
      path.resolve(__dirname, "RuntimeImageSelect.tsx"),
      "utf8",
    );
    expect(src).toMatch(/const isLocal = mode === "local"/);
    expect(src).toMatch(/if \(isLocal\) \{[\s\S]{0,600}return \(/);
    // An absent mode must NOT be treated as local — otherwise a caller that forgot the prop would
    // silently disable the picker for container runs.
    expect(src).not.toMatch(/mode !== "runtime"/);
  });
});

describe("the recurring forms pass the mode into the image picker", () => {
  // Without `mode`, the component falls back to "unknown" and behaves as a plain picker — which is
  // exactly the state that produced the report. These assertions are what stop that returning.
  it("the create form passes mode to RuntimeImageSelect", () => {
    const src = read(NEW_FORM);
    expect(src).toMatch(/<RuntimeImageSelect[\s\S]{0,400}mode=\{/);
    // "inherit" must be resolved against the chosen project, or a local project still shows a picker.
    expect(src).toMatch(/executionMode === "inherit"[\s\S]{0,120}protoToExecutionMode/);
  });

  it("the edit form passes mode to RuntimeImageSelect", () => {
    const src = read(EDIT_FORM);
    expect(src).toMatch(/<RuntimeImageSelect[\s\S]{0,400}mode=\{/);
    expect(src).toMatch(/executionMode === "inherit"[\s\S]{0,200}protoToExecutionMode/);
  });

  it("both forms disclose what local mode means, not just the picker label", () => {
    // The picker says "no image"; the disclosure says what a local run actually gets (host process, your
    // own filesystem). The regular forms show it, so the recurring ones must too.
    expect(read(NEW_FORM)).toContain("LocalModeDisclosure");
    expect(read(EDIT_FORM)).toContain("LocalModeDisclosure");
  });
});
