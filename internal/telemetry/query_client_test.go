package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/telemetry"
)

// vmHandler records the PromQL query string it was asked to run and returns a
// canned success response with a single series. The body keys off the metric
// name so a test can customize the returned values per name.
func vmHandler(t *testing.T, gotQuery *string, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestQueryMetricsSumsTokenClassCounters guards the cache-aware counters ADR
// regression: orchicon_tokens_consumed / orchicon_cost_usd now carry a
// token_class label (up to four series per name), so a bare label select no
// longer represents a total. QueryMetrics must wrap those names in sum(...) so
// a consumer reading a single latest point (the telemetry "Metric values"
// panel) still sees the whole-token/whole-cost total, not one arbitrary bucket.
func TestQueryMetricsSumsTokenClassCounters(t *testing.T) {
	var gotQuery string
	srv := vmHandler(t, &gotQuery, `{"status":"success","data":{"result":[{"metric":{"__name__":"orchicon_tokens_consumed"},"values":[[1000,"7"],[1010,"9"]]}]}}`)
	c := telemetry.NewQueryClient("", "", srv.URL)

	res, err := c.QueryMetrics(context.Background(), "tnt_dev", telemetry.MetricFilter{
		Names: []string{"orchicon_tokens_consumed"},
	})
	if err != nil {
		t.Fatalf("QueryMetrics: %v", err)
	}
	if !strings.Contains(gotQuery, "sum(orchicon_tokens_consumed{") {
		t.Fatalf("tokens query not wrapped in sum(): %q", gotQuery)
	}
	if !strings.Contains(gotQuery, `orchicon_tenant_id="tnt_dev"`) {
		t.Fatalf("tokens query lost tenant filter: %q", gotQuery)
	}
	if res.Degraded {
		t.Fatal("expected non-degraded result")
	}
	if len(res.Series) != 1 {
		t.Fatalf("series = %d, want 1 (sum collapses buckets to a single series)", len(res.Series))
	}
	pts := res.Series[0].Points
	if len(pts) != 2 {
		t.Fatalf("points = %d, want 2", len(pts))
	}
	// Newest-first ordering: the latest sample (value 9) is Points[0].
	if pts[0].Value != 9 {
		t.Fatalf("newest sample = %v, want 9", pts[0].Value)
	}
}

// TestQueryMetricsSumsCostPercentSame checks orchicon_cost_usd is treated like
// the token counter (the ADR splits cost by token_class the same way).
func TestQueryMetricsSumsCostPercentSame(t *testing.T) {
	var gotQuery string
	srv := vmHandler(t, &gotQuery, `{"status":"success","data":{"result":[{"metric":{"__name__":"orchicon_cost_usd"},"values":[[1000,"0.25"]]}]}}`)
	c := telemetry.NewQueryClient("", "", srv.URL)

	if _, err := c.QueryMetrics(context.Background(), "tnt_dev", telemetry.MetricFilter{
		Names: []string{"orchicon_cost_usd"},
	}); err != nil {
		t.Fatalf("QueryMetrics: %v", err)
	}
	if !strings.Contains(gotQuery, "sum(orchicon_cost_usd{") {
		t.Fatalf("cost query not wrapped in sum(): %q", gotQuery)
	}
}

// TestQueryMetricsLeavesUnsplitCountersUnwrapped ensures a metric that does NOT
// carry a token_class label (e.g. orchicon_outbox_lag) is left as a plain label
// select, and that a project-scoped filter is preserved.
func TestQueryMetricsLeavesUnsplitCountersUnwrapped(t *testing.T) {
	var gotQuery string
	srv := vmHandler(t, &gotQuery, `{"status":"success","data":{"result":[{"metric":{"__name__":"orchicon_outbox_lag"},"values":[[1000,"1"]]}]}}`)
	c := telemetry.NewQueryClient("", "", srv.URL)

	if _, err := c.QueryMetrics(context.Background(), "tnt_dev", telemetry.MetricFilter{
		Names:     []string{"orchicon_outbox_lag"},
		ProjectID: "prj_dev",
	}); err != nil {
		t.Fatalf("QueryMetrics: %v", err)
	}
	if strings.Contains(gotQuery, "sum(") {
		t.Fatalf("unsplit metric was wrapped in sum(): %q", gotQuery)
	}
	if !strings.Contains(gotQuery, `project="prj_dev"`) {
		t.Fatalf("project filter lost: %q", gotQuery)
	}
}
