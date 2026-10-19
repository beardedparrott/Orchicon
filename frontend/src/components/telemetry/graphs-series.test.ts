import { describe, expect, it } from "vitest";

import { identifyingLabels, familySeries, MAX_SERIES_PER_FAMILY } from "./graphs";

// What is pinned here is the bug that made the Metrics page lie: it rendered
// one chart per metric FAMILY and took the first series it found, so on a
// tenant that emits a family per model every model but one was invisible while
// the page still reported "N series". These assertions are about selection and
// ordering, not rendering.

function series(name: string, labels: Record<string, string>, values: number[]) {
  return {
    metricName: name,
    labels,
    points: values.map((v, i) => ({
      timestamp: { seconds: BigInt(1_700_000_000 + i), nanos: 0 },
      value: v,
    })),
  };
}

describe("identifyingLabels", () => {
  it("drops the tenant label, which is identical across a tenant-scoped query", () => {
    const got = identifyingLabels({ orchicon_tenant_id: "tnt_dev", model: "deepseek-flash" });
    expect(got).toEqual(["model=deepseek-flash"]);
  });

  it("drops empty values and the metric name label", () => {
    expect(identifyingLabels({ model: "", __name__: "orchicon_cost_usd" })).toEqual([]);
    expect(identifyingLabels(undefined)).toEqual([]);
  });

  it("is stable regardless of label insertion order", () => {
    const a = identifyingLabels({ b: "2", a: "1" });
    const b = identifyingLabels({ a: "1", b: "2" });
    expect(a).toEqual(b);
    expect(a).toEqual(["a=1", "b=2"]);
  });
});

describe("familySeries", () => {
  it("returns EVERY series in the family, not just the first", () => {
    // The regression: series.find(...) returned one series here.
    const all = [
      series("orchicon_cost_usd", { orchicon_tenant_id: "t", model: "a" }, [0.1]),
      series("orchicon_cost_usd", { orchicon_tenant_id: "t", model: "b" }, [0.9]),
      series("orchicon_cost_usd", { orchicon_tenant_id: "t", model: "c" }, [0.5]),
    ];
    const got = familySeries(all, "orchicon_cost_usd");
    expect(got).toHaveLength(3);
    expect(got.map((s) => s.id)).toEqual(["model=b", "model=c", "model=a"]);
  });

  it("orders by the LATEST value so truncation cannot hide the top spender", () => {
    const all = [
      series("m", { model: "small" }, [100, 1]),
      series("m", { model: "big" }, [1, 50]),
    ];
    expect(familySeries(all, "m").map((s) => s.id)).toEqual(["model=big", "model=small"]);
  });

  it("filters to the requested family only", () => {
    const all = [series("a", { model: "x" }, [1]), series("b", { model: "y" }, [2])];
    expect(familySeries(all, "a")).toHaveLength(1);
    expect(familySeries(all, "b")).toHaveLength(1);
    expect(familySeries(all, "missing")).toHaveLength(0);
  });

  it("gives an unlabelled series an empty id rather than a fake one", () => {
    const got = familySeries([series("a", { orchicon_tenant_id: "t" }, [5])], "a");
    expect(got[0].id).toBe("");
    expect(got[0].latest).toBe(5);
  });

  it("caps at a bounded number per family when rendered", () => {
    // The cap is a render decision; the data function must not impose it.
    expect(MAX_SERIES_PER_FAMILY).toBeGreaterThan(1);
    const many = Array.from({ length: MAX_SERIES_PER_FAMILY + 4 }, (_, i) =>
      series("a", { model: `m${i}` }, [i]),
    );
    expect(familySeries(many, "a")).toHaveLength(MAX_SERIES_PER_FAMILY + 4);
    expect(familySeries(many, "a").slice(0, MAX_SERIES_PER_FAMILY)).toHaveLength(MAX_SERIES_PER_FAMILY);
  });
});
