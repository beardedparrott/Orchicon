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
	//
	// A LIST IS A FACT: the model catalog changes when the operator installs a provider or edits
	// opencode's config, which is rare, and re-running the CLI costs a subprocess plus a service round
	// trip. Caching it amortises that across the many reads a picker makes.
	successTTL time.Duration

	// failureBackoff is how long a FAILURE is remembered before another attempt is allowed.
	//
	// A FAILURE IS A CONDITION, NOT A FACT — which is the whole reason this is separate from
	// successTTL. It exists to stop a subprocess being spawned on every call during a genuine outage;
	// it is not a claim about the world. Reusing the success TTL here was a defect with a measured
	// cost: opencode's managed service evicts and lazily re-boots a location, and a listing that lands
	// in that window returns an empty list. That blip was then served as the answer for FIVE MINUTES,
	// on a cold cache (which every rebuild creates) with no prior list to fall back on — and the
	// picker's retry key re-read the same cached error, so it could not retry. Reported as "I rebuilt
	// and it's still saying no models".
	//
	// Seconds rather than minutes: long enough to absorb a retry storm, short enough that the next
	// attempt — or the operator pressing `r` — is answered by a real probe.
	failureBackoff time.Duration

	// cliMu guards the resolved MODEL-LIST ARGV, which is a VERSION question. `models --verbose`
	// is what produces the metadata this package parses, and opencode 2.x REMOVED the flag: on
	// 2.0.26 the command accepts only `--standalone`/`--server` (packages/cli/src/commands/
	// commands.ts), so the flag is an unknown option and the CLI exits 1. Probe, then remember —
	// but only remember on SUCCESS, so a transient failure cannot permanently downgrade a v1 CLI
	// to the metadata-free form.
	cliMu       sync.Mutex
	cliOK       bool
	cliResolved bool

	// modelsArgsOverride is the surface's preferred models argv, set from the DETECTED API shape so
	// this file needs no knowledge of versions. v2 needs `--standalone` and v1 needs `--verbose`;
	// which one applies is a property of the shape, not of the parser.
	modelsArgsOverride []string
}

// modelsArgv returns the argument list to list models with.
//
// IT IS RESOLVED ONCE, and the reason is a real version difference rather than tidiness. opencode
// 1.x lists models as `opencode models [provider] [--refresh] [--verbose]`, and `--verbose` is what
// emits the per-model metadata blocks this package parses. opencode 2.x kept the command but dropped
// the flag — 2.0.26's CLI declares `models` with only the server params — so on a 2.x install the
// documented invocation is an UNKNOWN OPTION and the CLI exits 1. Reported against WSL opencode
// 2.0.26 as: `internal: list opencode models: opencode models: exit status 1`.
//
// The first call always tries the metadata form, because that is the richer one and the only one
// that works on 1.x; the fallback is recorded so a 2.x install pays for the failed attempt once
// rather than on every refresh.
func (d *ModelDiscoverer) modelsArgv() (argv []string, preferred bool) {
	d.cliMu.Lock()
	defer d.cliMu.Unlock()
	if d.cliResolved && !d.cliOK {
		return []string{"models"}, false
	}
	return append([]string(nil), d.modelsArgsOverride...), true
}

// rememberModelsForm records whether the surface's PREFERRED models form was accepted. ONLY A
// SUCCESS IS REMEMBERED: caching a failure would turn one transient error into a permanent downgrade
// — on v1 that silently loses every model's cost and context window, and on v2 it drops the
// --standalone flag that keeps the command from starting a background daemon.
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
// DETECTED api surface requires (opencode.ModelsArgs). Empty restores the v1 default. It exists
// because the version rule belongs to the adapter package and this one must not re-derive it.
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
		// Failure backoff: a recent refresh failed; surface the error without re-spawning the
		// subprocess on every call. SECONDS, not the success TTL — see failureBackoff for the measured
		// cost of conflating the two.
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

// emptyRetryAttempts and emptyRetryDelay are the model discoverer's names for the SHARED CLI retry policy
// (cliresilience.go). They are aliases rather than their own constants, and that is the point: two copies of
// "five attempts over ~4s" is how the model list and the MCP config came to be retried by different rules in
// the first place. The numbers and their reasoning now live in one place; these keep the existing call sites
// and messages readable.
const (
	emptyRetryAttempts = cliRetryAttempts
	emptyRetryDelay    = cliRetryDelay
)

// fetchModels runs the model listing for this surface, RETRYING the transient empty result.
func (d *ModelDiscoverer) fetchModels(ctx context.Context) ([]*apiv1.OpenCodeModel, error) {
	// A GENEROUS TIMEOUT, because on opencode 2.x this command may have to REACH A BACKGROUND SERVICE
	// that is not running yet. The first call after a boot can spend real time getting that service
	// up, and that cost is not a failure — it is the price of the only path that has a model list
	// (see apiShape.modelsArgs: the --standalone alternative is fast and EMPTY). The earlier 30s was
	// chosen before that was understood, and would report a spurious discovery failure on exactly the
	// first, cold call.
	//
	// It stays BOUNDED rather than unlimited: a wedged service must produce an error the operator can
	// read, not a picker that says "loading…" forever. And the result is cached (5 min TTL), so the
	// cost is paid once rather than per read.
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// THE WORKING DIRECTORY IS PINNED, and that is a determinism fix rather than a guess about the
	// reported failure. This call used to set no Dir at all, so the CLI ran in whatever directory the
	// PLANE inherited — which is the directory the operator happened to run the installer from (a
	// /mnt/c Windows path is a real case on WSL). The MCP discoverer beside this one has always
	// pinned it to the home dir, and an opencode invocation can be project-sensitive, so leaving it
	// unpinned means the model list could depend on where someone stood when they installed.
	//
	// The home dir is the right pin because it is where opencode's own config and auth live.
	argv, preferred := d.modelsArgv()

	// RETRIED THROUGH THE SHARED POLICY (cliresilience.go), so the model catalog and the MCP config are
	// retried by the SAME rule rather than by two copies of it. That is what this loop used to be: a private
	// retry that the sibling discoverer did not share, which is how one path ends up hardened and the other
	// not. The behaviour is unchanged — an empty catalog is still the transient case (errNoModels), and a
	// deterministic failure is still returned immediately — the policy simply lives in one place now.
	//
	// emptyIsTransient is TRUE: an empty model list is the documented mid-boot race, not an answer.
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

// fetchModelsOnce is one attempt: the surface's preferred form, then the plainest one if the preferred
// was rejected outright (a version that lacks the flag). An EMPTY list is not a rejection and does not
// reach the fallback — it is returned for the caller to retry.
func (d *ModelDiscoverer) fetchModelsOnce(ctx context.Context, argv []string, preferred bool) ([]*apiv1.OpenCodeModel, error) {
	out, err := runOpencodeCLI(ctx, d.binary, homeDir(), argv...)
	if err == nil {
		// RECORD ONLY WHAT WE ACTUALLY TRIED. An earlier version recorded `true` on any success,
		// so once the form had been downgraded the NEXT plain success re-enabled it and the choice
		// OSCILLATED — measured as the preferred form attempted twice across three fetches, i.e. a
		// 2.x install paying forever for a rejection it had already seen.
		if preferred {
			d.rememberModelsForm(true)
		}
		return parseModelOutput(out)
	}

	// The preferred form may simply not exist on this CLI. Retry with the plainest one, which every
	// version accepts; the metadata-free format is still a usable model list, because
	// parseModelOutput emits a bare ref as a model rather than dropping it.
	//
	// The fallback is only worth running when it DIFFERS from what just failed: on opencode 2.x the
	// preferred form IS `models`, so retrying it would run the same command twice — and each run can
	// reach the background service. Nothing is lost by skipping it, because the failure we already
	// hold is the one from that exact invocation.
	//
	// The plainest form is spelled here rather than shared: it carries no flag BY DEFINITION, so it is
	// not a version fact and does not belong in the shape table. This package also deliberately does
	// not import internal/opencode — the plane wires the two together (SetModelsArgs) so the version
	// rule stays in one place.
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
	// failure, because `models` alone is the thing that cannot work, and its error carries the CLI's
	// own words.
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
			// A REF WITH NO METADATA IS STILL A MODEL.
			//
			// This used to discard it, which made the metadata-free output — the ONLY output
			// opencode 2.x can produce, since it removed `--verbose` — parse to zero models and
			// then fail with "no models found in opencode output". The refs are the whole point
			// of the list; what is missing is cost/limits, and a model the operator can select
			// without seeing its price is much better than a picker with no models in it.
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
			// WRAPS errNoModels so the caller retries it: measured to be a TRANSIENT service-race
			// result far more often than a real absence of providers. The message still names the
			// non-transient possibility, because after the retries are exhausted that is what it means.
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
// metadata-free form of `opencode models` provides. Id and ProviderId are split on the first "/",
// matching how opencode names its refs; Name falls back to the id so a picker has something to
// show. Cost, Limits and Capabilities stay nil, which callers must already tolerate — they do,
// because parseModelJSON leaves them nil whenever the CLI omits them.
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
