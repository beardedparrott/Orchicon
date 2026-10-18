import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import type { ActionDTO, ResolutionDTO, StatusDTO } from "./desktop";
import {
  describeAction,
  needsInstance,
  nextView,
  pickerOffered,
  resolveChoice,
  stateLabel,
} from "./manage-server";

// manage-server.test.ts — AC5/AC6/AC7/AC9/AC10/AC12's pure half.

const status = (over: Partial<StatusDTO> = {}): StatusDTO => ({
  Instance: "dev",
  ControlPort: 8080,
  URL: "http://localhost:8080/healthz",
  State: "offline",
  Detail: "nothing answers http://localhost:8080/healthz",
  Residency: "host",
  PID: 0,
  Binary: "",
  Drift: "identity unknown",
  Healthy: false,
  LogPath: "/home/u/.local/share/orchicon-dev/serve/logs/orchicon.log",
  ...over,
});

const action = (over: Partial<ActionDTO> = {}): ActionDTO => ({
  Instance: "dev",
  Action: "start",
  Outcome: "started",
  OK: true,
  LogPath: "/home/u/.local/share/orchicon-dev/serve/logs/orchicon.log",
  Message: "host plane ready",
  ...over,
});

describe("the four connection states", () => {
  it("distinguishes connecting, connected, offline and plane-error", () => {
    expect(nextView(null, null, false).state).toBe("connecting");
    expect(nextView(null, null, true).state).toBe("connecting");
    expect(nextView(null, status({ State: "connected", Healthy: true }), false).state).toBe("connected");
    expect(nextView(null, status({ State: "offline" }), false).state).toBe("offline");
    expect(nextView(null, status({ State: "plane-error", Detail: "answers HTTP 503" }), false).state).toBe("plane-error");

    const labels = new Set(["connecting", "connected", "offline", "plane-error"].map((s) => stateLabel(s as never)));
    expect(labels.size).toBe(4);
  });

  it("keeps plane-error distinct from offline — the detail says what was FOUND", () => {
    const offline = nextView(null, status({ State: "offline" }), false);
    const sick = nextView(null, status({ State: "plane-error", Detail: "answers HTTP 503 at http://localhost:8080/healthz" }), false);
    expect(sick.state).not.toBe(offline.state);
    expect(sick.detail).toContain("503");
    expect(offline.detail).toContain("nothing answers");
  });

  it("never shows a stale connected state while an action is in flight", () => {
    const connected = nextView(null, status({ State: "connected", Healthy: true }), false);
    expect(connected.state).toBe("connected");
    const inFlight = nextView(connected, status({ State: "connected", Healthy: true }), true);
    expect(inFlight.state).toBe("connecting");
  });
});

describe("focus moves only when offline APPEARS", () => {
  it("targets the primary action on entering offline", () => {
    expect(nextView(null, status({ State: "offline" }), false).focusTarget).toBe("primary");
    expect(nextView(null, status({ State: "connected" }), false).focusTarget).toBeNull();
    const fromConnected = nextView({ state: "connected", detail: "", focusTarget: null }, status({ State: "offline" }), false);
    expect(fromConnected.focusTarget).toBe("primary");
  });

  it("does not steal focus on a repeat poll of the same offline state", () => {
    const prev = { state: "offline" as const, detail: "", focusTarget: "primary" as const };
    expect(nextView(prev, status({ State: "offline" }), false).focusTarget).toBeNull();
  });
});

describe("instance selection", () => {
  const unknownPort: ResolutionDTO = { Instance: "", ControlPort: 9999, Known: false, Running: [] };
  const known: ResolutionDTO = { Instance: "prod", ControlPort: 8091, Known: true, Running: [] };

  it("asks when the control port is not one it recognises", () => {
    expect(needsInstance(unknownPort)).toBe(true);
    expect(resolveChoice(undefined, unknownPort)).toEqual({ instance: null, ask: true, running: [] });
  });

  it("uses the shell's instance when it named one", () => {
    expect(resolveChoice("test", known).instance).toBe("test");
    expect(needsInstance(known)).toBe(false);
    expect(resolveChoice(undefined, known).instance).toBe("prod");
  });

  it("offers a picker when more than one plane is running", () => {
    const running: ResolutionDTO = {
      Instance: "dev",
      ControlPort: 8080,
      Known: true,
      Running: [status({ Instance: "dev", State: "connected" }), status({ Instance: "prod", ControlPort: 8091, State: "connected" })],
    };
    expect(pickerOffered(running)).toBe(true);
    expect(pickerOffered(known)).toBe(false);
    expect(pickerOffered(null)).toBe(false);
    expect(resolveChoice(undefined, running).running.length).toBe(2);
  });
});

describe("action outcomes", () => {
  it("names the instance in every confirmation", () => {
    for (const outcome of ["started", "already-running", "stopped", "already-stopped", "left-alone-unidentified", "failed"]) {
      expect(describeAction(action({ Outcome: outcome, OK: outcome !== "failed" }))).toContain("dev");
    }
  });

  it("reports the log path on failure and never claims success", () => {
    const msg = describeAction(action({ Outcome: "failed", OK: false }));
    expect(msg).toContain("failed");
    expect(msg).toContain("/home/u/.local/share/orchicon-dev/serve/logs/orchicon.log");
  });

  it("says what actually happened, including leaving an unidentifiable plane alone", () => {
    expect(describeAction(action({ Outcome: "already-running" }))).toContain("already running");
    expect(describeAction(action({ Outcome: "left-alone-unidentified" }))).toContain("left alone");
    expect(describeAction(action({ Outcome: "started" }))).toContain("answering /healthz");
  });
});

describe("the page's structure and its forbidden copy", () => {
  const src = readFileSync(new URL("../routes/manage-server.tsx", import.meta.url), "utf8");

  it("is sectioned, with a stated way to add a section", () => {
    expect(src).toContain("const SECTIONS: Section[]");
    expect(src).toContain("A SECTION IS ADDED BY PUSHING AN ENTRY INTO SECTIONS");
    expect(src).toContain("SECTIONS.map(");
  });

  it("renders NO explanatory copy about the desktop app (AC2/AC4)", () => {
    const forbidden = [
      "desktop app",
      "only available in the desktop",
      "not available in the browser",
      "use the desktop",
      "browser cannot",
      "available on desktop",
    ];
    const lower = src.toLowerCase();
    for (const phrase of forbidden) {
      expect(lower).not.toContain(phrase.toLowerCase());
    }
  });

  it("gates the route through the one desktop signal", () => {
    expect(src).toContain("redirectTarget(location.pathname, isDesktop())");
  });

  it("uses design-system primitives only — no second visual language", () => {
    expect(src).toContain('from "@/components/ui/button"');
    expect(src).toContain('from "@/components/ui/card"');
    expect(src).toContain('from "@/components/ui/badge"');
    // No hand-rolled palette: a user moving between clients must not be able to tell
    // the content area apart.
    expect(src).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    expect(src).not.toContain("style={{");
  });

  it("disables every action while in flight", () => {
    const disabled = src.match(/disabled=\{pending/g) ?? [];
    expect(disabled.length).toBeGreaterThanOrEqual(3);
  });

  it("polls the plane only while the window is visible", () => {
    expect(src).toContain('document.visibilityState === "hidden"');
  });
});
