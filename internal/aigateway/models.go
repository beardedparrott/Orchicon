package aigateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ModelDiscoverer discovers models by shelling out to the `opencode` CLI
// (docs/04 §6). Caching strategy (2026-09: the probe costs ~1.2s — a full
// CLI subprocess spawn parsing ~470 models — so reads never block on it
// once a list exists):
//
//   - Fresh cache: served instantly (the only blocking read is the cold
//     first load).
//   - TTL expired: STALE-WHILE-REVALIDATE — the stale list is served
//     immediately and a single-flight background refresh runs on a
//     detached bounded context. New models appear without any caller
//     paying the subprocess latency; the next read picks them up.
//   - Provider-landscape changes out-of-band (custom provider CRUD,
//     provider token saves) call Invalidate() so the next read refreshes
//     immediately — "new models usable right away" is an invalidation
//     contract, not a TTL wait.
//   - A failed refresh backs off for the TTL window (the last error is
//     surfaced without re-spawning the probe on every call).
type ModelDiscoverer struct {
	log    *slog.Logger
	binary string // path to opencode binary

	mu       sync.Mutex
	cache    []*apiv1.OpenCodeModel
	cached   time.Time
	failedAt time.Time // zero when the last refresh succeeded (or none ran)
	lastErr  error     // surfaced during the failure backoff window
	// Single-flight coordination: at most one subprocess probe runs at a
	// time; concurrent cold loads WAIT on the channel instead of erroring.
	refreshing  bool
	refreshDone chan struct{}
	ttl         time.Duration
}

// NewModelDiscoverer creates a discoverer that shells out to the opencode
// binary at the given path. If binary is empty, exec.LookPath finds it.
func NewModelDiscoverer(log *slog.Logger, binary string) *ModelDiscoverer {
	if binary == "" {
		binary = "opencode"
	}
	return &ModelDiscoverer{
		log:    log.With("component", "model_discoverer"),
		binary: binary,
		ttl:    5 * time.Minute,
	}
}

// ListModels returns all models from opencode, using a cached result if
// fresh. Provider filter narrows results to a single provider.
func (d *ModelDiscoverer) ListModels(ctx context.Context, provider string) ([]*apiv1.OpenCodeModel, error) {
	models, err := d.fetchOrCache(ctx)
	if err != nil {
		return nil, err
	}
	if provider == "" {
		return models, nil
	}
	filtered := make([]*apiv1.OpenCodeModel, 0, len(models))
	for _, m := range models {
		if strings.EqualFold(m.ProviderId, provider) {
			filtered = append(filtered, m)
		}
	}
	return filtered, nil
}

// Invalidate drops the cached model list so the NEXT read triggers a
// single-flight background refresh. Call it whenever the provider
// landscape changes out-of-band (custom provider created/deleted, provider
// token saved/cleared, opencode config edited): freshly added models
// become discoverable immediately instead of after the TTL.
func (d *ModelDiscoverer) Invalidate() {
	d.mu.Lock()
	d.cached = time.Time{}
	d.mu.Unlock()
}

// fetchOrCache returns the model list under the SWR contract documented on
// the struct. The ctx bounds a COLD first load; background revalidations
// run on their own detached context (the caller's ctx may end with the
// request).
func (d *ModelDiscoverer) fetchOrCache(ctx context.Context) ([]*apiv1.OpenCodeModel, error) {
	d.mu.Lock()
	if d.cache != nil && time.Since(d.cached) < d.ttl {
		models := d.cache
		d.mu.Unlock()
		return models, nil
	}
	if d.refreshing {
		// A probe is in flight: cold loads WAIT for it (they have nothing
		// to serve), warm loads fall through to stale below.
		wait := d.refreshDone
		haveStale := d.cache != nil
		stale := d.cache
		d.mu.Unlock()
		if haveStale {
			return stale, nil
		}
		// The probe can take up to its 30s timeout; honor caller
		// cancellation while waiting for it.
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return d.afterWait(ctx)
	}
	if d.cache != nil {
		// TTL expired (or invalidated): serve stale instantly and refresh
		// single-flighted in the background.
		stale := d.cache
		d.startRefreshLocked()
		d.mu.Unlock()
		go d.refresh(context.Background())
		return stale, nil
	}
	// Cold cache with no probe in flight: the first load is synchronous
	// (callers need real rows; a picker must not show nothing).
	if d.failedAt.After(time.Time{}) && d.lastErr != nil && time.Since(d.failedAt) < d.ttl {
		// Failure backoff: a recent refresh failed; surface the error
		// without re-spawning the subprocess on every call.
		err := d.lastErr
		d.mu.Unlock()
		return nil, err
	}
	d.startRefreshLocked()
	d.mu.Unlock()
	return d.refresh(ctx)
}

// afterWait re-reads the cache state after a cold-load waiter was released
// by the refresh completing.
func (d *ModelDiscoverer) afterWait(ctx context.Context) ([]*apiv1.OpenCodeModel, error) {
	d.mu.Lock()
	if d.cache != nil {
		models := d.cache
		d.mu.Unlock()
		return models, nil
	}
	// The winner failed and there was never a cache: surface the error
	// (the backoff contract still applies on the NEXT call).
	err := d.lastErr
	if err == nil {
		err = fmt.Errorf("opencode model discovery: refresh produced no models")
	}
	d.mu.Unlock()
	return nil, err
}

// startRefreshLocked arms a single-flight refresh. Caller holds the lock.
func (d *ModelDiscoverer) startRefreshLocked() {
	d.refreshing = true
	d.refreshDone = make(chan struct{})
}

// finishRefreshLocked completes the in-flight marker. Caller holds the
// lock. Returns the done channel to broadcast (close wakes cold-load
// waiters).
func (d *ModelDiscoverer) finishRefreshLocked() chan struct{} {
	d.refreshing = false
	ch := d.refreshDone
	d.refreshDone = nil
	return ch
}

// refresh shells out once and stores the result. Callers must NOT hold
// the lock; ctx bounds the subprocess call (cold loads pass the caller's
// ctx, background revalidations pass a detached one). The refreshDone
// channel is closed on completion to wake any cold-load waiters.
func (d *ModelDiscoverer) refresh(ctx context.Context) ([]*apiv1.OpenCodeModel, error) {
	models, err := d.fetchModels(ctx)
	d.mu.Lock()
	done := d.finishRefreshLocked()
	if err != nil {
		d.failedAt = time.Now()
		d.lastErr = err
		if d.cache != nil {
			// Keep serving stale; stamp the timestamp so a broken CLI does
			// not cause a spawn-per-call stampede (backoff = TTL). Capture
			// the slice under the lock — it must not be read after unlock.
			stale := d.cache
			d.cached = time.Now()
			d.mu.Unlock()
			close(done)
			d.log.Warn("failed to refresh models from opencode, serving stale cache", "error", err)
			return stale, nil
		}
		d.mu.Unlock()
		close(done)
		return nil, err
	}
	d.cache = models
	d.cached = time.Now()
	d.failedAt = time.Time{}
	d.lastErr = nil
	d.mu.Unlock()
	close(done)
	return models, nil
}

// fetchModels shells out to `opencode models --verbose` and parses the
// output into structured proto messages.
func (d *ModelDiscoverer) fetchModels(ctx context.Context) ([]*apiv1.OpenCodeModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.binary, "models", "--verbose")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("opencode models: %w", err)
	}
	return parseModelOutput(out)
}

// parseModelOutput parses the output of `opencode models --verbose`.
// The format alternates between model ref lines (e.g. "anthropic/claude-sonnet-4")
// and pretty-printed JSON objects with full metadata.
// Example:
//
//	anthropic/claude-sonnet-4
//	{
//	  "id": "claude-sonnet-4",
//	  "providerID": "anthropic",
//	  ...
//	}
func parseModelOutput(data []byte) ([]*apiv1.OpenCodeModel, error) {
	var models []*apiv1.OpenCodeModel
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(data, len(data))

	var currentRef string
	var jsonBuf bytes.Buffer
	bracing := -1 // brace depth; -1 = not in JSON

	flush := func() error {
		if currentRef == "" || jsonBuf.Len() == 0 {
			return nil
		}
		m, err := parseModelJSON(currentRef, jsonBuf.Bytes())
		if err != nil {
			// Skip unparseable model entries rather than failing entirely.
			jsonBuf.Reset()
			currentRef = ""
			bracing = -1
			return nil
		}
		models = append(models, m)
		jsonBuf.Reset()
		currentRef = ""
		bracing = -1
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			continue
		}

		if bracing < 0 {
			// Not inside a JSON block. Check if this looks like a model ref.
			if strings.Contains(trimmed, "/") && !strings.HasPrefix(trimmed, "{") {
				// Flush previous entry before starting new one.
				if err := flush(); err != nil {
					return nil, err
				}
				currentRef = trimmed
				continue
			}
			// Could be start of JSON for the very first model.
			if strings.HasPrefix(trimmed, "{") {
				bracing = 1
				jsonBuf.WriteString(line)
				jsonBuf.WriteByte('\n')
				continue
			}
			// Skip unexpected lines (warnings, etc.)
			continue
		}

		// Inside a JSON block — track brace depth.
		jsonBuf.WriteString(line)
		jsonBuf.WriteByte('\n')
		for _, ch := range line {
			switch ch {
			case '{':
				bracing++
			case '}':
				bracing--
			}
		}
		if bracing == 0 {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}

	// Flush any remaining entry.
	if err := flush(); err != nil {
		return nil, err
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan model output: %w", err)
	}

	if len(models) == 0 {
		return nil, fmt.Errorf("no models found in opencode output")
	}
	return models, nil
}

// rawModel is the JSON shape from `opencode models --verbose`.
type rawModel struct {
	ID          string         `json:"id"`
	ProviderID  string         `json:"providerID"`
	Name        string         `json:"name"`
	Family      string         `json:"family"`
	Status      string         `json:"status"`
	Cost        *rawCost       `json:"cost,omitempty"`
	Limit       *rawLimit      `json:"limit,omitempty"`
	Capabs      *rawCapabs     `json:"capabilities,omitempty"`
	ReleaseDate string         `json:"release_date"`
	Variants    map[string]any `json:"variants,omitempty"`
}

type rawCost struct {
	Input       float64    `json:"input"`
	Output      float64    `json:"output"`
	Cache       *rawCache  `json:"cache,omitempty"`
	Experimental any      `json:"experimentalOver200K,omitempty"`
	Tiers       []any      `json:"tiers,omitempty"`
}

type rawCache struct {
	Read  float64 `json:"read"`
	Write float64 `json:"write"`
}

type rawLimit struct {
	Context int64 `json:"context"`
	Input   int64 `json:"input"`
	Output  int64 `json:"output"`
}

type rawCapabs struct {
	Temperature bool           `json:"temperature"`
	Reasoning   bool           `json:"reasoning"`
	Attachment  bool           `json:"attachment"`
	Toolcall    bool           `json:"toolcall"`
	Input       *rawIO         `json:"input,omitempty"`
	Output      *rawIO         `json:"output,omitempty"`
	Interleaved any            `json:"interleaved"`
}

type rawIO struct {
	Text  bool `json:"text"`
	Audio bool `json:"audio"`
	Image bool `json:"image"`
	Video bool `json:"video"`
	PDF   bool `json:"pdf"`
}

func parseModelJSON(ref string, data []byte) (*apiv1.OpenCodeModel, error) {
	var raw rawModel
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse model %q: %w", ref, err)
	}

	m := &apiv1.OpenCodeModel{
		Id:          raw.ID,
		ProviderId:  raw.ProviderID,
		Name:        raw.Name,
		Family:      raw.Family,
		Status:      raw.Status,
		ModelRef:    ref,
		ReleaseDate: raw.ReleaseDate,
	}

	if raw.Cost != nil {
		m.Cost = &apiv1.ModelCost{
			Input:  raw.Cost.Input,
			Output: raw.Cost.Output,
		}
		if raw.Cost.Cache != nil {
			m.Cost.CacheRead = raw.Cost.Cache.Read
			m.Cost.CacheWrite = raw.Cost.Cache.Write
		}
	}

	if raw.Limit != nil {
		m.Limits = &apiv1.ModelLimits{
			Context: raw.Limit.Context,
			Input:   raw.Limit.Input,
			Output:  raw.Limit.Output,
		}
	}

	if raw.Capabs != nil {
		m.Capabilities = &apiv1.ModelCapabilities{
			Temperature: raw.Capabs.Temperature,
			Reasoning:   raw.Capabs.Reasoning,
			Attachment:  raw.Capabs.Attachment,
			Toolcall:    raw.Capabs.Toolcall,
		}
		if raw.Capabs.Input != nil {
			m.Capabilities.InputText = raw.Capabs.Input.Text
			m.Capabilities.InputImage = raw.Capabs.Input.Image
			m.Capabilities.InputPdf = raw.Capabs.Input.PDF
			m.Capabilities.InputAudio = raw.Capabs.Input.Audio
			m.Capabilities.InputVideo = raw.Capabs.Input.Video
		}
		if raw.Capabs.Output != nil {
			m.Capabilities.OutputText = raw.Capabs.Output.Text
		}
		m.Capabilities.Interleaved = raw.Capabs.Interleaved != false && raw.Capabs.Interleaved != nil
	}

	for variant := range raw.Variants {
		m.Variants = append(m.Variants, variant)
	}

	return m, nil
}


// Ensure the discoverer respects context deadlines.
var _ = (*ModelDiscoverer)(nil)
var _ = timestamppb.Now // keep import
