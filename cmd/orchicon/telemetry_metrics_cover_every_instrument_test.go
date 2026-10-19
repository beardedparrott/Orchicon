package main

// telemetry_metrics_cover_every_instrument_test.go — THE METRICS PAGE MUST LIST
// EVERY INSTRUMENT THE PLANE EMITS.
//
// THE GAP THIS CLOSES, from the operator: "there aren't any new metrics I can
// see on the dashboard". Two causes, and this is the second one. The first was
// a broken collector (both instances demanded :8888, so nothing shipped at all —
// see TestCollectorDeclaresItsSelfMetricsListener). The second was this list:
//
//	frontend/src/components/telemetry/graphs.tsx
//	export const TELEMETRY_METRICS = [
//	  "orchicon_tokens_consumed",
//	  "orchicon_cost_usd",
//	  "orchicon_outbox_lag",
//	];
//
// The control plane declares SEVENTEEN instruments — recovery seeds, dispatch
// deferrals, prefix-cache, session recycle, outbox age and alerts. All of them
// were already being shipped to VictoriaMetrics and none of them could be seen,
// because the page only ever asked for three. A metric nobody queries does not
// exist, as far as an operator is concerned.
//
// The failure mode is silent in both directions: the Go side compiles, the front
// end typechecks, and nothing anywhere asserts that the two halves agree on a
// set of names. So it is asserted here, against the real source of both halves.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// instrumentCall matches an OTel instrument constructor. The NAME is the first
// string literal after it — which is how every call site in internal/ is
// written (with or without a metric.WithDescription option argument).
var instrumentCall = regexp.MustCompile(
	`(?:Int64Counter|Float64Counter|Int64Gauge|Float64Gauge|Int64UpDownCounter|Float64UpDownCounter|` +
		`Int64Histogram|Float64Histogram|Int64ObservableGauge|Float64ObservableGauge|` +
		`Int64ObservableCounter|Float64ObservableCounter)\(\s*"([a-z0-9_]+)"`)

// declaredInstruments walks the Go sources and returns every metric name the
// plane registers with the OTel meter.
func declaredInstruments(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..", "internal")
	out := map[string]string{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range instrumentCall.FindAllStringSubmatch(string(b), -1) {
			if !strings.HasPrefix(m[1], "orchicon_") {
				continue
			}
			out[m[1]] = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("found no OTel instruments in internal/ — the matcher is wrong, and a matcher that " +
			"finds nothing would make this test vacuously pass")
	}
	return out
}

// queriedMetrics reads the frontend's list of metric families.
func queriedMetrics(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "frontend", "src", "components", "telemetry", "graphs.tsx"))
	if err != nil {
		t.Fatalf("read graphs.tsx: %v", err)
	}
	src := string(b)

	idx := strings.Index(src, "export const TELEMETRY_METRICS")
	if idx < 0 {
		t.Fatal("graphs.tsx has no TELEMETRY_METRICS list")
	}
	start := strings.Index(src[idx:], "[")
	if start < 0 {
		t.Fatal("TELEMETRY_METRICS has no array")
	}
	rest := src[idx+start:]
	end := strings.Index(rest, "];")
	if end < 0 {
		t.Fatal("TELEMETRY_METRICS array is unterminated")
	}
	block := rest[:end]

	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([a-z0-9_]+)"`).FindAllStringSubmatch(block, -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatal("TELEMETRY_METRICS parsed to an empty list")
	}
	return out
}

// TestMetricsPageQueriesEveryEmittedInstrument is the sweep: add an instrument
// in Go and the Metrics page must learn about it, or this fails.
func TestMetricsPageQueriesEveryEmittedInstrument(t *testing.T) {
	declared := declaredInstruments(t)
	queried := queriedMetrics(t)

	var missing []string
	for name := range declared {
		if !queried[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		var paths []string
		for _, name := range missing {
			paths = append(paths, name+" ("+declared[name]+")")
		}
		t.Errorf("the plane emits %d instrument(s) the Metrics page never queries:\n  %s\n\n"+
			"A metric nobody asks for does not exist as far as an operator is concerned — that is exactly\n"+
			"\"there aren't any new metrics I can see\". Add each name to TELEMETRY_METRICS (and to\n"+
			"METRIC_UNITS) in frontend/src/components/telemetry/graphs.tsx.",
			len(missing), strings.Join(paths, "\n  "))
	}
}

// TestMetricsPageDoesNotQueryInstrumentsThatDoNotExist is the other half: a name
// in the list with no instrument behind it is a permanently empty chart, which
// reads as "the feature is broken" rather than "nothing emitted".
func TestMetricsPageDoesNotQueryInstrumentsThatDoNotExist(t *testing.T) {
	declared := declaredInstruments(t)
	queried := queriedMetrics(t)

	var phantom []string
	for name := range queried {
		if _, ok := declared[name]; !ok {
			// The outbox gauges are Int64ObservableGauge, matched above; anything
			// left is genuinely undeclared.
			phantom = append(phantom, name)
		}
	}
	sort.Strings(phantom)

	if len(phantom) > 0 {
		t.Errorf("TELEMETRY_METRICS names %d metric(s) no instrument declares: %s\n"+
			"Each would render as a permanently empty chart.",
			len(phantom), strings.Join(phantom, ", "))
	}
}
