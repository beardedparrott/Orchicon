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
// (docs/04 §6). It caches results with a TTL so repeated fetches don't
// hammer the subprocess. Like OpenChamber, Orchicon discovers models
// dynamically from the opencode registry rather than hardcoding them.
type ModelDiscoverer struct {
	log    *slog.Logger
	binary string // path to opencode binary

	mu     sync.RWMutex
	cache  []*apiv1.OpenCodeModel
	cached time.Time
	ttl    time.Duration
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

func (d *ModelDiscoverer) fetchOrCache(ctx context.Context) ([]*apiv1.OpenCodeModel, error) {
	d.mu.RLock()
	if d.cache != nil && time.Since(d.cached) < d.ttl {
		d.mu.RUnlock()
		return d.cache, nil
	}
	d.mu.RUnlock()

	d.mu.Lock()
	defer d.mu.Unlock()

	// Double-check under write lock.
	if d.cache != nil && time.Since(d.cached) < d.ttl {
		return d.cache, nil
	}

	models, err := d.fetchModels(ctx)
	if err != nil {
		// On error, return stale cache if available rather than failing.
		if d.cache != nil {
			d.log.Warn("failed to refresh models from opencode, using stale cache", "error", err)
			return d.cache, nil
		}
		return nil, err
	}
	d.cache = models
	d.cached = time.Now()
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

// MockModelDiscoverer returns a discoverer that always returns the
// hardcoded provider/ model list without shelling out. Useful for
// dev mode when the opencode binary is absent.
func MockModelDiscoverer(log *slog.Logger) *ModelDiscoverer {
	d := &ModelDiscoverer{
		log:    log.With("component", "model_discoverer"),
		binary: "",
		ttl:    24 * time.Hour,
	}
	now := time.Now()
	d.cache = mockModels()
	d.cached = now
	return d
}

func mockModels() []*apiv1.OpenCodeModel {
	return []*apiv1.OpenCodeModel{
		{
			Id: "claude-sonnet-4", ProviderId: "anthropic", Name: "Claude Sonnet 4", Family: "claude-sonnet",
			Status: "active", ModelRef: "anthropic/claude-sonnet-4", ReleaseDate: "2025-06-15",
			Cost: &apiv1.ModelCost{Input: 3, Output: 15, CacheRead: 0.3},
			Limits: &apiv1.ModelLimits{Context: 200000, Output: 64000},
			Capabilities: &apiv1.ModelCapabilities{
				Temperature: true, Reasoning: true, Attachment: true, Toolcall: true,
				InputText: true, InputImage: true, InputPdf: true, OutputText: true,
			},
			Variants: []string{"none", "low", "medium", "high"},
		},
		{
			Id: "claude-opus-4", ProviderId: "anthropic", Name: "Claude Opus 4", Family: "claude-opus",
			Status: "active", ModelRef: "anthropic/claude-opus-4", ReleaseDate: "2025-06-15",
			Cost: &apiv1.ModelCost{Input: 15, Output: 75, CacheRead: 1.5},
			Limits: &apiv1.ModelLimits{Context: 200000, Output: 64000},
			Capabilities: &apiv1.ModelCapabilities{
				Temperature: false, Reasoning: true, Attachment: true, Toolcall: true,
				InputText: true, InputImage: true, InputPdf: true, OutputText: true,
			},
			Variants: []string{"medium", "high", "xhigh"},
		},
		{
			Id: "deepseek-v4-flash-free", ProviderId: "opencode", Name: "DeepSeek V4 Flash (Free)", Family: "deepseek",
			Status: "active", ModelRef: "opencode/deepseek-v4-flash-free", ReleaseDate: "2025-08-01",
			Cost: &apiv1.ModelCost{Input: 0, Output: 0},
			Limits: &apiv1.ModelLimits{Context: 128000, Output: 32000},
			Capabilities: &apiv1.ModelCapabilities{
				Temperature: true, Reasoning: true, Attachment: true, Toolcall: true,
				InputText: true, InputImage: true, InputPdf: false, OutputText: true,
			},
			Variants: []string{"low", "medium", "high"},
		},
	}
}

// Ensure the discoverer respects context deadlines.
var _ = (*ModelDiscoverer)(nil)
var _ = timestamppb.Now // keep import
