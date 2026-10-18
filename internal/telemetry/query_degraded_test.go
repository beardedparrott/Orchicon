package telemetry_test

// query_degraded_test.go — AC 5 of the native Telemetry page: a degraded
// query must NAME the backend that is down. Before this, every store
// failure produced Degraded=true with no reason, so the UI could only draw
// an empty chart and claim "no data".

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/telemetry"
)

// TestDegradedQueriesNameTheirBackend: with no endpoints configured, all
// three signals degrade and each reason names its own store.
func TestDegradedQueriesNameTheirBackend(t *testing.T) {
	ctx := context.Background()
	c := telemetry.NewQueryClient("", "", "")

	tr, _ := c.QueryTraces(ctx, "tnt_dev", telemetry.TraceFilter{})
	if !tr.Degraded || !strings.Contains(tr.Reason, "tempo") {
		t.Fatalf("traces: want degraded naming tempo, got degraded=%v reason=%q", tr.Degraded, tr.Reason)
	}

	mr, _ := c.QueryMetrics(ctx, "tnt_dev", telemetry.MetricFilter{Names: []string{"orchicon_outbox_lag"}})
	if !mr.Degraded || !strings.Contains(mr.Reason, "victoriametrics") {
		t.Fatalf("metrics: want degraded naming victoriametrics, got degraded=%v reason=%q", mr.Degraded, mr.Reason)
	}

	lr, _ := c.QueryLogs(ctx, "tnt_dev", telemetry.LogFilter{})
	if !lr.Degraded || !strings.Contains(lr.Reason, "loki") {
		t.Fatalf("logs: want degraded naming loki, got degraded=%v reason=%q", lr.Degraded, lr.Reason)
	}
}

// TestDegradedQueryNamesUnreachableEndpoint: a configured-but-dead backend
// reports the endpoint and the transport error, not just "degraded".
func TestDegradedQueryNamesUnreachableEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	url := srv.URL
	srv.Close() // nothing listens now

	c := telemetry.NewQueryClient(url, url, url)
	lr, _ := c.QueryLogs(context.Background(), "tnt_dev", telemetry.LogFilter{})
	if !lr.Degraded {
		t.Fatal("want degraded for an unreachable loki")
	}
	if !strings.Contains(lr.Reason, "loki") || !strings.Contains(lr.Reason, url) {
		t.Fatalf("reason must name the backend and endpoint, got %q", lr.Reason)
	}
}

// TestQueryLogsAppliesIdentityAndSearchFilters pins the LogQL selector: the
// project filter used to be populated by the service and silently dropped
// when the selector was built, and free-text search did not exist.
func TestQueryLogsAppliesIdentityAndSearchFilters(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer srv.Close()

	c := telemetry.NewQueryClient("", srv.URL, "")
	if _, err := c.QueryLogs(context.Background(), "tnt_dev", telemetry.LogFilter{
		ProjectID:   "prj_1",
		ExecutionID: "exe_1",
		WorkItemID:  "wi_1",
		Severity:    "ERROR",
		Search:      `boom "quoted"`,
	}); err != nil {
		t.Fatalf("query logs: %v", err)
	}
	for _, want := range []string{
		`orchicon_tenant_id="tnt_dev"`,
		`severity_number="17"`,
		`| project_id="prj_1"`,
		`| execution_id="exe_1"`,
		`| task_id="wi_1"`,
		`|= "boom \"quoted\""`,
	} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("selector missing %s\n got: %s", want, gotQuery)
		}
	}
}
