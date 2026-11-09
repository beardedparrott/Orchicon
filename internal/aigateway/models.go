package aigateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

	// successTTL is how long a SUCCESSFUL list is trusted.
	successTTL time.Duration

	// failureBackoff is how long a FAILURE is remembered before another attempt is allowed.
	failureBackoff time.Duration

	// cliMu guards the resolved MODEL-LIST ARGV, which is a VERSION question.
	cliMu       sync.Mutex
	cliOK       bool
	cliResolved bool

	// modelsArgsOverride is the surface's preferred models argv, set from the DETECTED API shape so
	// this file needs no knowledge of versions.
	modelsArgsOverride []string
}

// modelsArgv returns the argument list to list models with.
func (d *ModelDiscoverer) modelsArgv() (argv []string, preferred bool) {
	d.cliMu.Lock()
	defer d.cliMu.Unlock()
	if d.cliResolved && !d.cliOK {
		return []string{"models"}, false
	}
	return append([]string(nil), d.modelsArgsOverride...), true
}

// ONLY A SUCCESS IS REMEMBERED: caching a failure would turn one transient error into a permanent
// downgrade — on v1 that silently loses every model's cost and context window.
func (d *ModelDiscoverer) rememberModelsForm(preferredOK bool) {
	d.cliMu.Lock()
	defer d.cliMu.Unlock()
	if !preferredOK && d.cliResolved {
		return // already downgraded; nothing to change
	}
	d.cliOK = preferredOK
	d.cliResolved = true
}

// NewModelDiscoverer creates a discoverer that shells out to the opencode
// binary at the given path. If binary is empty, exec.LookPath finds it.
func NewModelDiscoverer(log *slog.Logger, binary string) *ModelDiscoverer {
	if binary == "" {
		binary = "opencode"
	}
	return &ModelDiscoverer{
		log:            log.With("component", "model_discoverer"),
		binary:         binary,
		successTTL:     5 * time.Minute,
		failureBackoff: 10 * time.Second,
		// The DEFAULT is the v1 form, so a caller that never sets one behaves exactly as before.
		modelsArgsOverride: []string{"models", "--verbose"},
	}
}

// SetModelsArgs overrides the argv used to list models, so the caller can hand in the form the
// DETECTED api surface requires (opencode.ModelsArgs).
func (d *ModelDiscoverer) SetModelsArgs(args []string) {
	d.cliMu.Lock()
	defer d.cliMu.Unlock()
	if len(args) == 0 {
		d.modelsArgsOverride = []string{"models", "--verbose"}
		return
	}
	d.modelsArgsOverride = append([]string(nil), args...)
}

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

// Invalidate drops the cached model list so the NEXT read triggers a single-flight background
// refresh.
func (d *ModelDiscoverer) Invalidate() {
	d.mu.Lock()
	d.cached = time.Time{}
	d.mu.Unlock()
}

// fetchOrCache returns the model list under the SWR contract documented on the struct.
func (d *ModelDiscoverer) fetchOrCache(ctx context.Context) ([]*apiv1.OpenCodeModel, error) {
	d.mu.Lock()
	if d.cache != nil && time.Since(d.cached) < d.successTTL {
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
	if d.failedAt.After(time.Time{}) && d.lastErr != nil && time.Since(d.failedAt) < d.failureBackoff {
		// Failure backoff: a recent refresh failed; surface the error without re-spawning the subprocess on
		// every call.
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

// finishRefreshLocked completes the in-flight marker.
func (d *ModelDiscoverer) finishRefreshLocked() chan struct{} {
	d.refreshing = false
	ch := d.refreshDone
	d.refreshDone = nil
	return ch
}

// refresh shells out once and stores the result.
func (d *ModelDiscoverer) refresh(ctx context.Context) ([]*apiv1.OpenCodeModel, error) {
	models, err := d.fetchModels(ctx)
	d.mu.Lock()
	done := d.finishRefreshLocked()
	if err != nil {
		d.failedAt = time.Now()
		d.lastErr = err
		if d.cache != nil {
			// Keep serving stale; stamp the timestamp so a broken CLI does not cause a spawn-per-call stampede
			// (backoff = TTL).
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
// errNoModels marks the ONE discovery failure that is worth RETRYING: the command SUCCEEDED and
// printed nothing.
//
// WHY IT IS RETRIABLE, measured rather than assumed. opencode's managed service EVICTS and lazily
// re-boots a location, and `opencode models` short-circuits to an empty list when its request lands in
// that window. opencode's own log shows the eviction and the re-boot:
//
//	"location services evicted"   directory=<the host home directory>
//	"location services booted"    durationMs=23
//
// and the CLI is otherwise perfectly reliable: 20 consecutive runs returned 18,648 bytes in ~65ms
// each, while ONE run taken inside the boot window returned nothing. So an empty list is NOT a
// statement about the providers — it is a RACE, and treating it as definitive is what turned a
// sub-second window into a five-minute outage.
//
// WHY THAT MATTERS MORE THAN IT SOUNDS: the failure is cached (fetchOrCache's backoff), and a plane
// restart — which every rebuild does — starts from a COLD cache. So the first picker open after a
// rebuild is the most likely one to land in the window, and it then sticks for the full TTL with no
// prior list to fall back on, while pressing the retry key re-reads the same cached error. Reported as
// "I rebuilt and it's still saying no models", having worked minutes earlier.
//
// A "printed something unparseable" failure is deliberately NOT this: that is deterministic, so
// retrying it only delays a real diagnosis.
var errNoModels = errors.New("opencode listed no models")

// emptyRetryAttempts and emptyRetryDelay are the model discoverer's names for the SHARED CLI retry
// policy (cliresilience.go).
const (
	emptyRetryAttempts = cliRetryAttempts
	emptyRetryDelay    = cliRetryDelay
)

// fetchModels runs the model listing for this surface, RETRYING the transient empty result.
func (d *ModelDiscoverer) fetchModels(ctx context.Context) ([]*apiv1.OpenCodeModel, error) {
	// A GENEROUS TIMEOUT, because on opencode 2.x this command may have to REACH A BACKGROUND SERVICE
	// that is not running yet.
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// THE WORKING DIRECTORY IS PINNED, and that is a determinism fix rather than a guess about the
	// reported failure.
	argv, preferred := d.modelsArgv()

	// RETRIED THROUGH THE SHARED POLICY (cliresilience.go), so the model catalog and the MCP config are
	// retried by the SAME rule rather than by two copies of it.
	var models []*apiv1.OpenCodeModel
	_, err := withCLIRetries(ctx, d.log, "models", true, func() (int, error) {
		got, ferr := d.fetchModelsOnce(ctx, argv, preferred)
		if ferr != nil {
			return 0, ferr
		}
		models = got
		return len(got), nil
	})
	if err != nil {
		return nil, err
	}
	return models, nil
}

// fetchModelsOnce is one attempt: the surface's preferred form, then the plainest one if the
// preferred was rejected outright (a version that lacks the flag).
func (d *ModelDiscoverer) fetchModelsOnce(ctx context.Context, argv []string, preferred bool) ([]*apiv1.OpenCodeModel, error) {
	out, err := runOpencodeCLI(ctx, d.binary, homeDir(), argv...)
	if err == nil {
		// RECORD ONLY WHAT WE ACTUALLY TRIED.
		if preferred {
			d.rememberModelsForm(true)
		}
		return parseModelOutput(out)
	}

	// The preferred form may simply not exist on this CLI.
	fallback := []string{"models"}
	if strings.Join(fallback, " ") != strings.Join(argv, " ") {
		plainOut, plainErr := runOpencodeCLI(ctx, d.binary, homeDir(), fallback...)
		if plainErr == nil {
			d.rememberModelsForm(false)
			return parseModelOutput(plainOut)
		}
		err = plainErr
	}

	// The form was rejected outright, so this is not a transient empty: report the PLAIN invocation's
	// failure, because `models` alone is the thing that cannot work.
	return nil, fmt.Errorf("list opencode models: %w", err)
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
		if currentRef == "" {
			return nil
		}
		if jsonBuf.Len() == 0 {
			// This used to discard it, which made the metadata-free output — the ONLY output opencode 2.x can
			// produce, since it removed `--verbose`.
			models = append(models, modelFromRef(currentRef))
			currentRef = ""
			bracing = -1
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
		// TWO VERY DIFFERENT SITUATIONS REACH HERE, and telling them apart is the whole value of the
		// message. opencode 2.x reports an empty model list by printing NOTHING and exiting 0 (its
		// handler ends with `if (models.length > 0) process.stdout.write(...)`), so "no models found"
		// was being reported for a command that SUCCEEDED — and it reads like a parse failure, which
		// sends the operator to the wrong place entirely.
		//
		// So the RAW OUTPUT is examined rather than assumed about:
		//   * nothing printed  -> a genuinely empty list, which in practice means no provider is
		//     authenticated for the environment this command ran in.
		//   * something printed -> the build emits a format this parser does not understand, and the
		//     snippet is included so that is visible without reproducing it by hand.
		//
		// The second case is the one no amount of guessing can resolve, so it carries its evidence.
		if printed := bytes.TrimSpace(data); len(printed) == 0 {
			// WRAPS errNoModels so the caller retries it: measured to be a TRANSIENT service-race result far
			// more often than a real absence of providers.
			return nil, fmt.Errorf("%w: the command succeeded and printed NOTHING, which is how it "+
				"reports an empty list — and which it does TRANSIENTLY when the managed service is "+
				"evicting or re-booting the location. Retried %d times without seeing a list, so the "+
				"remaining explanation is that no provider is authenticated for the environment the "+
				"plane runs in — check `opencode auth list` and run `opencode models` by hand in the "+
				"same shell", errNoModels, emptyRetryAttempts)
		} else {
			return nil, fmt.Errorf("opencode printed %d bytes but no models could be read from them — "+
				"this build's output format is not one this version understands. First %d bytes, "+
				"verbatim: %s", len(printed), min(300, len(printed)), excerpt(printed))
		}
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
	Input        float64   `json:"input"`
	Output       float64   `json:"output"`
	Cache        *rawCache `json:"cache,omitempty"`
	Experimental any       `json:"experimentalOver200K,omitempty"`
	Tiers        []any     `json:"tiers,omitempty"`
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
	Temperature bool   `json:"temperature"`
	Reasoning   bool   `json:"reasoning"`
	Attachment  bool   `json:"attachment"`
	Toolcall    bool   `json:"toolcall"`
	Input       *rawIO `json:"input,omitempty"`
	Output      *rawIO `json:"output,omitempty"`
	Interleaved any    `json:"interleaved"`
}

type rawIO struct {
	Text  bool `json:"text"`
	Audio bool `json:"audio"`
	Image bool `json:"image"`
	Video bool `json:"video"`
	PDF   bool `json:"pdf"`
}

// modelFromRef builds a model from a bare `provider/model` reference, which is all the
// metadata-free form of `opencode models` provides.
func modelFromRef(ref string) *apiv1.OpenCodeModel {
	id, provider := ref, ""
	if i := strings.Index(ref, "/"); i > 0 {
		provider, id = ref[:i], ref[i+1:]
	}
	return &apiv1.OpenCodeModel{
		Id:         id,
		ProviderId: provider,
		Name:       id,
		ModelRef:   ref,
	}
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
