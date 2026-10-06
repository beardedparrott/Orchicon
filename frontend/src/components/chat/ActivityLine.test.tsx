// ActivityLine.test.tsx — the presentational line, asserted by PARSED MARKUP rather than a source grep.
//
// renderToStaticMarkup (react-dom/server) is the repo's way to assert a component without jsdom
// (@testing-library/jsdom/happy-dom are NOT dependencies here); the pattern NoticeBubble.test.tsx
// already uses. Because the component is hook-free (props in, markup out), these are real attribute
// assertions, not approximations of them.
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { ActivityLine } from "./ActivityLine";

const markup = () =>
  renderToStaticMarkup(
    createElement(ActivityLine, {
      text: "Orchicon is contemplating… · 3 modifies · 1 read · last 4s",
      announcement: "Orchicon is working · 3 modifies · 1 read",
    }),
  );

describe("ActivityLine", () => {
  it("AC9 — is a polite status region, not an alert", () => {
    const html = markup();
    expect(html).toContain('role="status"');
    expect(html).toContain('aria-live="polite"');
    expect(html).toContain('data-testid="ask-activity-line"');
    expect(html).not.toContain('role="alert"');
    expect(html).not.toContain('aria-live="assertive"');
  });

  it("AC9 — the rotating text is SILENT to a screen reader; the stable announcement is not", () => {
    const html = markup();
    // The verb + counters are visual animation (15s rotation, 1s age): inside aria-hidden.
    const hiddenIdx = html.indexOf('aria-hidden="true"');
    const textIdx = html.indexOf("Orchicon is contemplating");
    expect(hiddenIdx).toBeGreaterThanOrEqual(0);
    expect(textIdx).toBeGreaterThan(hiddenIdx);
    // ...and the announced content is the STABLE string, in an sr-only span.
    expect(html).toContain('class="sr-only"');
    const srOnlyIdx = html.indexOf('class="sr-only"');
    expect(html.indexOf("Orchicon is working · 3 modifies · 1 read")).toBeGreaterThan(
      srOnlyIdx,
    );
  });

  it("keyboard/focus — the line is NOT in the tab order", () => {
    const html = markup();
    expect(html.toLowerCase()).not.toContain("tabindex");
    expect(html).not.toContain("<button");
    expect(html).not.toContain("<a ");
    expect(html).not.toContain("contenteditable");
  });

  it("AC8 — design-system primitives only, no hardcoded colour", () => {
    const html = markup();
    for (const token of [
      "text-muted-foreground",
      "bg-sky-500",
      "animate-pulse",
      "rounded-2xl",
      "border-sky-300/30",
      "bg-sky-50/20",
      "dark:border-sky-950/40",
      "dark:bg-sky-950/10",
      "motion-reduce:animate-none",
    ]) {
      expect(html).toContain(token);
    }
    expect(html).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    expect(html).not.toMatch(/rgb\(/);
  });

  it("carries the fallback-model annotation only when there is one", () => {
    const withModel = renderToStaticMarkup(
      createElement(ActivityLine, {
        text: "Orchicon is thinking…",
        announcement: "Orchicon is working",
        fallbackModel: "claude-x",
      }),
    );
    expect(withModel).toContain("claude-x");
    expect(withModel).toContain("free fallback, may be rate-limited");

    const without = renderToStaticMarkup(
      createElement(ActivityLine, {
        text: "Orchicon is thinking…",
        announcement: "Orchicon is working",
        fallbackModel: null,
      }),
    );
    expect(without).not.toContain("free fallback");
  });
});
