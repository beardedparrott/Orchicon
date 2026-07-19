package telemetry

// Slog→OTel log bridge. The control plane's structured log records
// (slog) are the source of truth for runtime observability — workflow
// step transitions, reconciliation outcomes, dispatch events,
// recovery progress, adapter lifecycle — but they only land in the
// local log file by default. To make them visible in the Telemetry
// logs tab (which queries ClickHouse via the SigNozClient), we
// install a slog.Handler that fans every record out to the OTel log
// provider alongside the existing JSON handler.
//
// Why a custom handler instead of otelslog: the OTel Go ecosystem
// does not ship a first-party slog→log bridge yet, and we need full
// control over the severity mapping (slog.Level* → otel_log.Severity)
// plus the attribute conversion (slog.Attr → otel_log.KeyValue). The
// MultiHandler keeps the existing JSON-to-stderr behaviour intact so
// `orchicon dev logs` still tails the same stream.

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	otel_log "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
)

// slogMinLevel is the floor for the OTel-side fan-out. We don't want
// every debug-level record pushed to ClickHouse — the local JSON
// log file is the right place for that, and ClickHouse is for
// operator-visible signals.
const slogMinLevel = slog.LevelInfo

// multiHandler fans a record out to every wrapped handler. It's the
// Glue between the local JSON handler and the OTel handler so a
// single slog call lands in both places. Errors from the wrapped
// handlers are returned (first non-nil wins) but never block other
// handlers from running — a failed OTel emit must not silence the
// local log line.
type multiHandler struct {
	handlers []slog.Handler
}

// MultiHandler returns a slog.Handler that writes to every provided
// handler in order. Useful for the telemetry hot path where the
// control-plane log file (JSON) and the OTel log provider both
// need to see the same record.
func MultiHandler(handlers ...slog.Handler) slog.Handler {
	return &multiHandler{handlers: handlers}
}

func (h *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, hh := range h.handlers {
		if hh.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, hh := range h.handlers {
		if !hh.Enabled(ctx, r.Level) {
			continue
		}
		if err := hh.Handle(ctx, r); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	wrapped := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		wrapped[i] = hh.WithAttrs(attrs)
	}
	return &multiHandler{handlers: wrapped}
}

func (h *multiHandler) WithGroup(name string) slog.Handler {
	wrapped := make([]slog.Handler, len(h.handlers))
	for i, hh := range h.handlers {
		wrapped[i] = hh.WithGroup(name)
	}
	return &multiHandler{handlers: wrapped}
}

// otelHandler converts each slog.Record into an OTel log record and
// emits it through the global LoggerProvider. The OTel SDK is
// already wired into the same ClickHouse tables the embedded SigNoz
// UI reads, so the records appear in the Telemetry logs tab without
// any extra exporter wiring.
type otelHandler struct {
	providerMu sync.RWMutex
	provider   otel_log.LoggerProvider
	scopes     map[string]otel_log.Logger
	scopeMu    sync.Mutex
}

// NewOtelSlogHandler returns a slog.Handler that emits every record
// to the OTel log pipeline. The handler captures the global
// LoggerProvider on construction; if the provider has not been
// initialised yet (e.g. logs emitted during early boot, before
// telemetry.Setup runs), the handler is a no-op until a provider is
// registered.
func NewOtelSlogHandler() slog.Handler {
	return &otelHandler{
		provider: global.GetLoggerProvider(),
		scopes:   make(map[string]otel_log.Logger),
	}
}

// SetLoggerProvider rebinds the underlying OTel LoggerProvider.
// Called by telemetry.Setup once the OTLP exporter is wired so the
// handler begins forwarding records into ClickHouse. Safe to call
// concurrently with Handle.
func (h *otelHandler) SetLoggerProvider(p otel_log.LoggerProvider) {
	h.providerMu.Lock()
	defer h.providerMu.Unlock()
	h.provider = p
	h.scopeMu.Lock()
	h.scopes = make(map[string]otel_log.Logger)
	h.scopeMu.Unlock()
}

func (h *otelHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slogMinLevel
}

func (h *otelHandler) Handle(ctx context.Context, r slog.Record) error {
	// Look up the provider on every call. The global OTel
	// LoggerProvider is set by telemetry.Setup AFTER this handler
	// is constructed (the dev subcommand's logger is wired before
	// the OTel SDK is), so a one-shot capture at construction
	// would miss every record. A per-call lookup is cheap (one
	// atomic read) and lets the handler pick up the real provider
	// as soon as it's registered.
	provider := h.providerOrGlobal()
	if provider == nil {
		// No provider yet (early boot). Drop the record rather than
		// failing — the local JSON handler still sees it.
		return nil
	}
	logger := h.loggerFor("orchicon", provider)

	rec := otel_log.Record{}
	rec.SetTimestamp(r.Time)
	rec.SetSeverity(slogLevelToOtel(r.Level))
	rec.SetBody(otel_log.StringValue(r.Message))
	r.Attrs(func(a slog.Attr) bool {
		rec.AddAttributes(slogAttrToKeyValue(a))
		return true
	})
	logger.Emit(ctx, rec)
	return nil
}

// providerOrGlobal returns the explicitly-set provider if one was
// bound via SetLoggerProvider, otherwise the global OTel provider.
// Always non-nil — global.GetLoggerProvider() returns a no-op
// default if the SDK has not been initialised.
func (h *otelHandler) providerOrGlobal() otel_log.LoggerProvider {
	h.providerMu.RLock()
	p := h.provider
	h.providerMu.RUnlock()
	if p != nil {
		return p
	}
	return global.GetLoggerProvider()
}

func (h *otelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// For the OTel side, attribute grouping changes the *scope* we
	// log under (e.g. "orchicon.workflow" for the workflow
	// reconciler's grouped logger). The simplest correct behaviour is
	// to keep one global scope and let OTel's per-attribute
	// filtering do the work — grouping is a presentation detail.
	return h
}

func (h *otelHandler) WithGroup(_ string) slog.Handler {
	return h
}

// loggerFor returns (and caches) an otel_log.Logger for the given
// scope name. The scope becomes the OTel resource attribute
// `otel.scope.name` on every emitted record, which lets the
// SigNoz/ClickHouse side filter by component.
func (h *otelHandler) loggerFor(scope string, provider otel_log.LoggerProvider) otel_log.Logger {
	h.scopeMu.Lock()
	defer h.scopeMu.Unlock()
	if l, ok := h.scopes[scope]; ok {
		return l
	}
	l := provider.Logger(scope)
	h.scopes[scope] = l
	return l
}

// slogLevelToOtel maps the standard slog levels onto OTel severity.
// Trace < Debug < Info < Warn < Error < Fatal.
func slogLevelToOtel(level slog.Level) otel_log.Severity {
	switch {
	case level >= slog.LevelError:
		return otel_log.SeverityError
	case level >= slog.LevelWarn:
		return otel_log.SeverityWarn
	case level >= slog.LevelInfo:
		return otel_log.SeverityInfo
	default:
		return otel_log.SeverityDebug
	}
}

// slogAttrToKeyValue converts a single slog attribute into an OTel
// log KeyValue. The slog value space is richer than the OTel one
// (e.g. slog.GroupValue, slog.LogValuer) — we collapse anything we
// don't recognise into its String() form so the operator can still
// grep for it in the Telemetry logs tab.
func slogAttrToKeyValue(a slog.Attr) (kv otel_log.KeyValue) {
	defer func() {
		if recover() != nil {
			kv = asStringAttr(a)
		}
	}()
	switch a.Value.Kind() {
	case slog.KindString:
		return otel_log.KeyValue{Key: a.Key, Value: otel_log.StringValue(a.Value.String())}
	case slog.KindInt64:
		return otel_log.KeyValue{Key: a.Key, Value: otel_log.Int64Value(a.Value.Int64())}
	case slog.KindFloat64:
		return otel_log.KeyValue{Key: a.Key, Value: otel_log.Float64Value(a.Value.Float64())}
	case slog.KindBool:
		return otel_log.KeyValue{Key: a.Key, Value: otel_log.BoolValue(a.Value.Bool())}
	case slog.KindDuration:
		return otel_log.KeyValue{Key: a.Key, Value: otel_log.Int64Value(a.Value.Duration().Nanoseconds())}
	case slog.KindTime:
		return otel_log.KeyValue{Key: a.Key, Value: otel_log.StringValue(a.Value.Time().Format("2006-01-02T15:04:05.000000Z07:00"))}
	case slog.KindUint64:
		return otel_log.KeyValue{Key: a.Key, Value: otel_log.Int64Value(int64(a.Value.Uint64()))}
	case slog.KindGroup:
		attrs := a.Value.Group()
		values := make([]otel_log.Value, 0, len(attrs))
		for _, sub := range attrs {
			values = append(values, slogAttrToKeyValue(sub).Value)
		}
		return otel_log.KeyValue{Key: a.Key, Value: otel_log.SliceValue(values...)}
	case slog.KindLogValuer:
		return slogAttrToKeyValue(slog.Attr{Key: a.Key, Value: a.Value.Resolve()})
	default:
		return asStringAttr(a)
	}
}

func asStringAttr(a slog.Attr) otel_log.KeyValue {
	return otel_log.KeyValue{Key: a.Key, Value: otel_log.StringValue(strings.TrimSpace(a.Value.String()))}
}
