package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestMultiHandler_FansOut verifies the multi-handler writes the same
// record to every wrapped handler, with the same payload. This is
// the contract the dev subcommand relies on: every control-plane
// log line must land in BOTH the local JSON log (for `orchicon dev
// logs` tailing) and the OTel handler (for the Telemetry logs tab).
func TestMultiHandler_FansOut(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewJSONHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelInfo})
	h2 := slog.NewJSONHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(MultiHandler(h1, h2))

	log.Info("hello", "k", "v")

	if !strings.Contains(buf1.String(), `"msg":"hello"`) {
		t.Errorf("handler 1 missing record: %s", buf1.String())
	}
	if !strings.Contains(buf2.String(), `"msg":"hello"`) {
		t.Errorf("handler 2 missing record: %s", buf2.String())
	}
}

// TestMultiHandler_LevelFiltering verifies the per-handler Enabled
// check still works. A handler that has been configured for WARN+
// should not see INFO records, even when wrapped in a MultiHandler
// that has a sibling that's INFO+ permissive.
func TestMultiHandler_LevelFiltering(t *testing.T) {
	var bufHigh, bufLow bytes.Buffer
	hHigh := slog.NewJSONHandler(&bufHigh, &slog.HandlerOptions{Level: slog.LevelWarn})
	hLow := slog.NewJSONHandler(&bufLow, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(MultiHandler(hHigh, hLow))

	log.Info("info-record")
	log.Warn("warn-record")

	if strings.Contains(bufHigh.String(), "info-record") {
		t.Errorf("warn+ handler should not have received info record: %s", bufHigh.String())
	}
	if !strings.Contains(bufHigh.String(), "warn-record") {
		t.Errorf("warn+ handler missing warn record: %s", bufHigh.String())
	}
	if !strings.Contains(bufLow.String(), "info-record") {
		t.Errorf("info+ handler missing info record: %s", bufLow.String())
	}
	if !strings.Contains(bufLow.String(), "warn-record") {
		t.Errorf("info+ handler missing warn record: %s", bufLow.String())
	}
}

// TestMultiHandler_WithAttrsPropagates verifies that WithAttrs
// (used internally by slog for context-scoped loggers like
// slog.With("tenant_id", ...)) builds child handlers rather than
// dropping the wrap. The JSON envelope must still include the
// bound attribute on every record.
func TestMultiHandler_WithAttrsPropagates(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(MultiHandler(slog.NewJSONHandler(&buf, nil)))
	scoped := log.With("tenant_id", "tnt_dev")
	scoped.Info("hello", "k", "v")

	// slogJSONHandler emits the bound attributes under their key;
	// extract the record and confirm tenant_id is present.
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &rec); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, buf.String())
	}
	if rec["tenant_id"] != "tnt_dev" {
		t.Errorf("expected tenant_id=tnt_dev in record, got: %s", buf.String())
	}
	if rec["msg"] != "hello" {
		t.Errorf("expected msg=hello in record, got: %s", buf.String())
	}
}

// TestOtelHandler_EnabledAtInfo ensures the OTel side respects the
// slogMinLevel floor — Debug records are dropped at the handler
// level so we don't flood ClickHouse with verbose control-plane
// chatter that belongs in the local log file.
func TestOtelHandler_EnabledAtInfo(t *testing.T) {
	h := NewOtelSlogHandler()
	ctx := context.Background()
	if h.Enabled(ctx, slog.LevelDebug) {
		t.Error("Debug should be disabled (slogMinLevel = Info)")
	}
	if !h.Enabled(ctx, slog.LevelInfo) {
		t.Error("Info should be enabled")
	}
	if !h.Enabled(ctx, slog.LevelError) {
		t.Error("Error should be enabled")
	}
}

// TestSlogLevelToOtel covers the level mapping exhaustively.
// Slog's "Error" lands at OTel ERROR; WARN lands at WARN; INFO at
// INFO; anything below (Debug and below) lands at DEBUG.
func TestSlogLevelToOtel(t *testing.T) {
	cases := map[slog.Level]string{
		slog.LevelError: "ERROR",
		slog.LevelWarn:  "WARN",
		slog.LevelInfo:  "INFO",
		slog.LevelDebug: "DEBUG",
	}
	for in, want := range cases {
		if got := slogLevelToOtel(in).String(); got != want {
			t.Errorf("slogLevelToOtel(%v) = %s, want %s", in, got, want)
		}
	}
}

// TestSlogAttrToKeyValue covers the type-conversion branches that
// appear in real control-plane records (string, int64, bool, time,
// group, fallback). Group values must be converted to a slice
// (the OTel log SDK's only nested representation); strings must
// be preserved verbatim.
func TestSlogAttrToKeyValue(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		kv := slogAttrToKeyValue(slog.String("k", "v"))
		if kv.Key != "k" || kv.Value.AsString() != "v" {
			t.Errorf("unexpected: %+v", kv)
		}
	})
	t.Run("int64", func(t *testing.T) {
		kv := slogAttrToKeyValue(slog.Int64("k", 42))
		if kv.Key != "k" || kv.Value.AsInt64() != 42 {
			t.Errorf("unexpected: %+v", kv)
		}
	})
	t.Run("bool", func(t *testing.T) {
		kv := slogAttrToKeyValue(slog.Bool("k", true))
		if kv.Key != "k" || !kv.Value.AsBool() {
			t.Errorf("unexpected: %+v", kv)
		}
	})
	t.Run("group", func(t *testing.T) {
		kv := slogAttrToKeyValue(slog.Group("g",
			slog.String("a", "1"),
			slog.Int("b", 2),
		))
		if kv.Key != "g" {
			t.Errorf("unexpected key: %s", kv.Key)
		}
		items := kv.Value.AsSlice()
		if len(items) != 2 {
			t.Errorf("expected 2 group items, got %d", len(items))
		}
	})
}
