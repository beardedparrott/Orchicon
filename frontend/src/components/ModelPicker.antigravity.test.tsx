import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

import { projectPickerModels } from "@/api/providers";
import { ProviderModel } from "@/api/gen/orchicon/api/v1/provider_pb";
import { formatModelRef, parseModelRef } from "@/lib/model-ref";
import { ANTIGRAVITY_NOT_INSTALLED_MESSAGE } from "@/lib/adapter-errors";

// The GUI model tier for the SECOND catalog-sourced non-native kind, `antigravity`.
//
// WHY THIS SUITE EXISTS ALONGSIDE ModelPicker.claude.test.tsx. `antigravity` reaches the picker by a
// DIFFERENT route in two places, and both are worth failing loudly if they regress:
//
//   1. Its provider tier is a ONE-ELEMENT set (`google`, from adapter.AddAdapterKind(KindAntigravity,
//      "google")) served by the ADAPTER-SCOPED gateway list, not the merged Providers view — so the
//      provider pill must appear with no `google` row in the tenant's providers table at all.
//   2. `google` has no built-in profile and no vendored catalog row, so its model tier used to fail with
//      `[not_found] db: not found` before the providers service grew the antigravity branch. The rows it
//      returns now are agy's OWN bare model ids (`gemini-4-carbon-high`), never composed refs.
//
// ModelPicker.test.tsx pins the component's SOURCE text; that cannot assert a non-empty list. This suite
// runs the REAL projection the picker's model tier consumes (projectPickerModels ←
// useProviderModelsForPicker) plus the REAL ref formatter, so "non-empty list" and "the exact
// antigravity/google/<model> ref" are actually exercised.
describe("ModelPicker antigravity model tier (catalog-sourced, agy rows)", () => {
  // What ProviderService.ListProviderModels(provider_id="google") returns for a live agy listing: agy's
  // bare model ids, source=live, with the observed context window and the effort suffix in the id.
  // (internal/providers/antigravity.go antigravityRowsFromLive.)
  const googleLiveRows = [
    new ProviderModel({
      id: "gemini-4-carbon-high",
      context: 1048576n,
      visible: true,
      source: "live",
      reasoning: true,
    }),
    new ProviderModel({
      id: "gemini-4-carbon-low",
      context: 1048576n,
      visible: true,
      source: "live",
      reasoning: true,
    }),
    new ProviderModel({
      id: "gemini-4-neon-low",
      context: 1048576n,
      visible: true,
      source: "live",
      reasoning: true,
    }),
  ];

  it("yields a NON-EMPTY model list for antigravity's only provider (google)", () => {
    const models = projectPickerModels("google", googleLiveRows);
    expect(models.length).toBeGreaterThan(0);
    expect(models.map((m) => m.id)).toEqual([
      "gemini-4-carbon-high",
      "gemini-4-carbon-low",
      "gemini-4-neon-low",
    ]);
    // Every row's VALUE is the bare model id — never a composed ref, or the picker's join would emit a
    // bogus 4-segment ref (the exact bug internal/providers/antigravity.go's doc comment names).
    expect(models.every((m) => !m.id.includes("/"))).toBe(true);
    expect(models.every((m) => m.providerId === "google")).toBe(true);
    // The context hint survives: the picker's compaction math depends on it, and agy's window is 1M.
    expect(Number(models[0].limits?.context ?? 0)).toBe(1048576);
  });

  it("commits exactly antigravity/google/<model>, round-tripping the grammar", () => {
    const models = projectPickerModels("google", googleLiveRows);
    const picked = models.find((m) => m.id === "gemini-4-neon-low");
    expect(picked).toBeDefined();
    // selectModel() joins the SELECTED adapter + provider + model.id.
    const ref = formatModelRef("antigravity", "google", picked!.id);
    expect(ref).toBe("antigravity/google/gemini-4-neon-low");
    // The plane parses that ref back to the same triple before dispatch.
    expect(parseModelRef(ref)).toMatchObject({
      adapter: "antigravity",
      provider: "google",
      model: "gemini-4-neon-low",
    });
    // Exactly three segments: a provider id containing "/" or a model id already carrying one would
    // produce four, and the plane rejects those.
    expect(ref.split("/")).toHaveLength(3);
  });

  it("keeps the snapshot source and its rows selectable (the degraded fallback still lists models)", () => {
    // The offline path (no agy on this host): the vendored snapshot, source=antigravity-snapshot. The
    // rows must still project, because a snapshot list is what the operator sees — the picker annotates it
    // "may be stale" rather than hiding it.
    const snapshotRows = [
      new ProviderModel({
        id: "gemini-4-carbon-high",
        context: 1048576n,
        visible: true,
        source: "antigravity-snapshot",
        reasoning: true,
      }),
    ];
    const models = projectPickerModels("google", snapshotRows);
    expect(models.length).toBe(1);
    const ref = formatModelRef("antigravity", "google", models[0].id);
    expect(parseModelRef(ref)).toMatchObject({
      adapter: "antigravity",
      provider: "google",
      model: "gemini-4-carbon-high",
    });
  });
});

// --- the adapter tier + the stale-selection guard, pinned on the SOURCE ------------------------------
//
// The reset contract is a BEHAVIOUR of the component's own handlers (selectAdapter / the seeding effect),
// and this repo ships no jsdom — so it is pinned where ModelPicker.adapter.test.tsx already pins the
// claude-scoped half of it: on the component's source text. What is asserted here is that nothing in
// that path is keyed on `claude`, i.e. that a second catalog-sourced kind reaches it for free.
describe("ModelPicker antigravity adapter tier (source contract)", () => {
  const src = fs.readFileSync(path.join(__dirname, "ModelPicker.tsx"), "utf8");

  it("classifies the model tier from the server's sourcing_kinds, never a hard-coded kind list", () => {
    // The ONE classification the picker makes. If this were `adapter === "claude"` an antigravity
    // selection would silently take the opencode-CLI discovery path and list nothing.
    expect(src).toContain("sourcingKinds ?? [ORCHICON_ADAPTER_KIND]");
    expect(src).toMatch(/const usesCatalogSourcing = \(sourcingKinds \?\? \[ORCHICON_ADAPTER_KIND\]\)\.includes\(adapter\)/);
    // And there is no claude-only branch hiding a new kind.
    expect(src).not.toMatch(/adapter === "claude"/);
    expect(src).not.toMatch(/adapter === 'claude'/);
  });

  it("drives the ADAPTER tier from the server's registered kinds, so `antigravity` appears without a code change", () => {
    expect(src).toContain("useListAdapterKinds");
    expect(src).toContain("adapterList.map");
    // The only kind hoisted to the front is the native one (a claim about the DEFAULT, which only the
    // binary-free kind can make).
    expect(src).toMatch(/\[ORCHICON_ADAPTER_KIND, \.\.\.adapterKinds\.filter/);
  });

  it("switching adapter RESETS provider + model and writes no ref (ADR-0004 stale-selection guard)", () => {
    expect(src).toContain("function selectAdapter");
    expect(src).toMatch(/setAdapter\(kind\);\s*setProvider\(""\);\s*setSearch\(""\);/);
    // No onChange inside selectAdapter: a ref is written only when a MODEL is chosen in the new scope.
    const fn = src.slice(src.indexOf("function selectAdapter"), src.indexOf("function selectProvider"));
    expect(fn).not.toContain("onChange(");
  });

  it("shows the install sentence in the model tier's empty state for antigravity", () => {
    // The state a host with no `agy` produces: no rows, no error. Rendering "No models match your search"
    // there tells the operator their search was too narrow and leaves them with no next step.
    expect(src).toContain("ANTIGRAVITY_NOT_INSTALLED_MESSAGE");
    expect(src).toContain('adapter === "antigravity" && !search.trim()');
  });

  it("surfaces the server's degraded bit as a 'may be stale' note beside the list", () => {
    expect(src).toContain("nativeModelsQ.degraded");
    expect(src).toContain("may be stale");
  });
});

describe("the antigravity install sentence", () => {
  it("names the CLI and the download URL", () => {
    expect(ANTIGRAVITY_NOT_INSTALLED_MESSAGE).toContain("`agy`");
    expect(ANTIGRAVITY_NOT_INSTALLED_MESSAGE).toContain("https://antigravity.google/download");
    // It is the INSTALL state, not the sign-in one: there is nothing to sign in to.
    expect(ANTIGRAVITY_NOT_INSTALLED_MESSAGE).not.toContain("sign in");
  });
});
