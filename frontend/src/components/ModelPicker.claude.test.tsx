import { describe, expect, it } from "vitest";

import { projectPickerModels } from "@/api/providers";
import { ProviderModel } from "@/api/gen/orchicon/api/v1/provider_pb";
import { formatModelRef, parseModelRef } from "@/lib/model-ref";

// The GUI model tier for the CATALOG-SOURCED kind `claude`. On a plane with no
// opencode binary the CLI path returns Unimplemented, so the picker must
// resolve the model list from the providers SOURCING view — which, with a dead
// probe, is the offline vendored-catalog seed.
//
// ModelPicker.test.tsx pins the component's SOURCE text; that cannot assert a
// non-empty list. This suite runs the REAL projection the picker's model tier
// consumes (projectPickerModels ← useProviderModelsForPicker) plus the REAL ref
// formatter, so "non-empty list" and "the exact claude/anthropic/<model> ref"
// are actually exercised.
describe("ModelPicker claude model tier (catalog-sourced, opencode-free plane)", () => {
  // Exactly what ProviderService.ListProviderModels returns for anthropic when
  // the live probe yields nothing: the vendored catalog's authored anthropic
  // entries, marked source=catalog.
  const anthropicCatalogRows = [
    new ProviderModel({
      id: "claude-sonnet-4",
      context: 200000n,
      visible: true,
      source: "catalog",
      reasoning: true,
    }),
    new ProviderModel({ id: "claude-opus-4", context: 200000n, visible: true, source: "catalog" }),
    new ProviderModel({ id: "claude-haiku-4", context: 200000n, visible: true, source: "catalog" }),
  ];

  it("yields a NON-EMPTY model list for claude's provider (anthropic)", () => {
    const models = projectPickerModels("anthropic", anthropicCatalogRows);
    expect(models.length).toBeGreaterThan(0);
    expect(models.map((m) => m.id)).toEqual([
      "claude-sonnet-4",
      "claude-opus-4",
      "claude-haiku-4",
    ]);
    // Every row's VALUE is the bare model id — never the legacy 2-segment
    // model_ref — or the picker's join would emit a bogus 4-segment ref.
    expect(models.every((m) => !m.id.includes("/"))).toBe(true);
    expect(models.every((m) => m.providerId === "anthropic")).toBe(true);
    // The context hint survives: the picker's compaction math depends on it.
    expect(Number(models[0].limits?.context ?? 0)).toBe(200000);
  });

  it("commits exactly claude/anthropic/<model>, round-tripping the grammar", () => {
    const models = projectPickerModels("anthropic", anthropicCatalogRows);
    const picked = models.find((m) => m.id === "claude-sonnet-4");
    expect(picked).toBeDefined();
    // selectModel() joins the SELECTED adapter + provider + model.id.
    const ref = formatModelRef("claude", "anthropic", picked!.id);
    expect(ref).toBe("claude/anthropic/claude-sonnet-4");
    // The plane parses that ref back to the same triple before dispatch.
    expect(parseModelRef(ref)).toMatchObject({
      adapter: "claude",
      provider: "anthropic",
      model: "claude-sonnet-4",
    });
  });
});
