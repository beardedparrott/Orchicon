package aigateway

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestEmitParityAdapterKindAndSession asserts the OTel metrics carry the
// adapter parity attribute (which adapter drove the model call) and the Ask
// session attribute (conversation id), so telemetry spans/logs/metrics identify
// the adapter used and the Ask session, matching the acceptance criteria.
func TestEmitParityAdapterKindAndSession(t *testing.T) {
	prev := otel.GetMeterProvider()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(mp)
	defer otel.SetMeterProvider(prev)

	m := newUsageMetrics(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.emit(context.Background(), &db.UsageRecordRow{
		TenantID:     "tnt_ask",
		AdapterKind:  "orbital",
		SessionID:    "conv_123",
		Provider:     "mockprov",
		Model:        "deepseek-v4-flash",
		PromptTokens: 100,
	})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	// Gather every attribute set across all token/cost data points.
	seenAdapter, seenSession := false, false
	for _, scope := range rm.ScopeMetrics {
		for _, mt := range scope.Metrics {
			switch d := mt.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					seenAdapter = seenAdapter || attrHas(dp.Attributes, "adapter_kind", "orbital")
					seenSession = seenSession || attrHas(dp.Attributes, "session", "conv_123")
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					seenAdapter = seenAdapter || attrHas(dp.Attributes, "adapter_kind", "orbital")
					seenSession = seenSession || attrHas(dp.Attributes, "session", "conv_123")
				}
			}
		}
	}
	if !seenAdapter {
		t.Fatal("OTel metric data points did not carry adapter_kind=orbital parity attribute")
	}
	if !seenSession {
		t.Fatal("OTel metric data points did not carry session=conv_123 Ask-session attribute")
	}
}

// TestEmitParityNoBlankAdapter verifies a sample without an adapter kind (a
// legacy row) never emits a blank adapter_kind attribute — it is omitted.
func TestEmitParityNoBlankAdapter(t *testing.T) {
	prev := otel.GetMeterProvider()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(mp)
	defer otel.SetMeterProvider(prev)

	m := newUsageMetrics(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.emit(context.Background(), &db.UsageRecordRow{
		TenantID: "tnt_dev", Provider: "anthropic", Model: "claude-sonnet-4",
		PromptTokens: 10,
	})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, mt := range scope.Metrics {
			switch d := mt.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					if attrHas(dp.Attributes, "adapter_kind", "") {
						t.Error("blank adapter_kind attribute emitted for a legacy row without one")
					}
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					if attrHas(dp.Attributes, "adapter_kind", "") {
						t.Error("blank adapter_kind attribute emitted for a legacy row without one")
					}
				}
			}
		}
	}
}

func attrHas(set attribute.Set, key, want string) bool {
	for _, kv := range set.ToSlice() {
		if kv.Key == attribute.Key(key) && kv.Value.AsString() == want {
			return true
		}
	}
	return false
}
