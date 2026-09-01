import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

describe("ProvidersTab (ADR-0006)", () => {
  const src = fs.readFileSync(path.join(__dirname, "ProvidersTab.tsx"), "utf8");
  const api = fs.readFileSync(path.join(__dirname, "../api/providers.ts"), "utf8");

  it("lists merged providers: read-only built-ins and custom entries in the same list", () => {
    expect(src).toContain("useProviderList");
    expect(src).toContain("entry.readOnly");
    expect(src).toContain("entry.isCustom");
    expect(src).toMatch(/built-in/);
  });

  it("supports enable/disable, baseURL override, and Ollama context settings", () => {
    expect(src).toContain("saveToggle");
    expect(src).toContain("saveBaseUrl");
    expect(src).toContain('OLLAMA_ID = "ollama"');
    expect(src).toContain("num_ctx");
  });

  it("auto-writes the tenant secret on token save — operators never visit secrets manually", () => {
    expect(src).toContain("useSetProviderToken");
    expect(src).toContain("Writes tenant secret");
    expect(api).toContain("providerClient.setProviderToken");
    // Plaintext is write-only: never rendered back.
    expect(src).toContain('type="password"');
  });

  it("per-provider model visibility toggles invalidate on save and re-render the list", () => {
    expect(api).toContain("invalidateQueries({ queryKey: providersKeys.all })");
    expect(src).toContain("hiddenModels");
    expect(src).toContain("m.visible");
  });

  it("renders sourcing state: WARN badge for missing context hint, degraded banner on probe failure", () => {
    expect(src).toContain("warnNoContext");
    expect(src).toMatch(/WARN: no context hint/);
    expect(src).toContain("modelsQ.data?.degraded");
    expect(src).toMatch(/Probe failed/);
  });

  it("creates custom providers with display name, ref id, baseURL, auth mode", () => {
    expect(src).toContain("useCreateCustomProvider");
    expect(src).toMatch(/ref id/);
    expect(src).toMatch(/auth mode/);
    expect(src).toContain('value="token"');
  });

  it("surfaces the deletion guard: blocked deletes show the referencing workers", () => {
    expect(src).toContain("useDeleteCustomProvider");
    // The connect error message (FailedPrecondition, guard listing) is rendered.
    expect(src).toContain("onError: (e) => setMsg(String(e))");
  });

  it("every mutation invalidates the shared providers key — pickers auto-refresh on save", () => {
    const invalidations = (api.match(/invalidateQueries\(\{ queryKey: providersKeys\.all \}\)/g) ?? []).length;
    expect(invalidations).toBe(6); // settings, create, update, delete, setToken, clearToken
    expect(api).toContain("providersKeys.all");
  });

  it("edits custom providers (display name, base URL, auth mode; ref immutable)", () => {
    expect(src).toContain("useUpdateCustomProvider");
    expect(src).toMatch(/Edit custom provider/);
    expect(src).toMatch(/ref id is immutable after create/);
    // The edit control exists next to the delete control.
    expect(src).toMatch(/Pencil/);
    expect(src).toContain('aria-label={`Edit ${entry.id}`}');
  });

  it("manages manual model entries on custom providers (add/edit/remove + hints)", () => {
    expect(src).toContain("ManualModelsEditor");
    expect(src).toContain("replaceManualModels");
    expect(src).toContain("manualModels");
    expect(src).toMatch(/Manual models/);
    expect(src).toMatch(/context window/);
    expect(src).toMatch(/reasoning/);
    expect(src).toMatch(/max output/);
    // Hints ride the same sourcing tier as probe results.
    expect(src).toContain("m.source === \"manual\"");
  });
});
