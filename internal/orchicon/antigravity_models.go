package orchicon

// antigravity_models.go — the VENDORED Antigravity (agy) model snapshot, and the honest boundary around it.
//
// WHY A SNAPSHOT AT ALL. agy has no `models` subcommand (verified — do not invent one): its model
// catalogue is the `models.availableModels` array of a `session/new` RESULT, so listing it live needs a
// full initialize → authenticate → session/new handshake (internal/antigravity/modellist.go). That
// handshake spawns a process and can fail for reasons that have nothing to do with authentication (a
// handshake timeout, an unwired binary). This file is the answer to exactly that case, and only that
// case: the providers model tier falls back to it WITH a WARN.
//
// IT IS NEVER THE ANSWER TO AN AUTH FAILURE. A signed-out host must surface
// adapter.AntigravityAuthRequiredMessage — a populated list would tell the operator the tier works when
// every turn against it would fail, which is worse than the `[not_found] db: not found` this replaces.
//
// PROVENANCE IS IN THE FILE, not in a comment here: antigravity_models.json's _meta records the source
// (handshake_auth.jsonl:8, agy 1.5.13) and the capture date, so a picker row can honestly say
// "snapshot" and an operator can see how old the list is.

import (
	"embed"
	"encoding/json"
	"sort"
	"sync"
)

//go:embed antigravity_models.json
var antigravityModelsFS embed.FS

// AntigravitySnapshotProvenance is the `source` value a model row carries when it came from the vendored
// snapshot rather than from a live handshake. The picker renders `source` verbatim, so this string is
// operator-facing.
const AntigravitySnapshotProvenance = "snapshot"

// antigravitySnapshotFile is the on-disk shape of antigravity_models.json.
type antigravitySnapshotFile struct {
	Meta struct {
		Description string `json:"description"`
		Source      string `json:"source"`
		Captured    string `json:"captured"`
		Note        string `json:"note"`
	} `json:"_meta"`
	Models []struct {
		ID               string   `json:"id"`
		Name             string   `json:"name"`
		ReasoningEfforts []string `json:"reasoning_efforts"`
	} `json:"models"`
}

var (
	antigravitySnapshotOnce sync.Once
	antigravitySnapshot     []ModelInfo
	antigravitySnapshotInfo antigravitySnapshotMeta
)

// antigravitySnapshotMeta carries the file's provenance out to callers that log or render it.
type antigravitySnapshotMeta struct {
	Source   string
	Captured string
}

// AntigravityModelSnapshot returns the vendored agy model list (bare `modelId`s, sorted, Visible=true,
// Provenance "snapshot"), or nil when the embedded document cannot be decoded.
//
// The returned slice is a COPY, so a caller cannot mutate the cached snapshot into disagreeing with the
// file every other caller reads.
func AntigravityModelSnapshot() []ModelInfo {
	loadAntigravitySnapshot()
	out := make([]ModelInfo, len(antigravitySnapshot))
	copy(out, antigravitySnapshot)
	return out
}

// AntigravityModelSnapshotMeta reports the snapshot's recorded source and capture date (for a WARN line:
// "falling back to the 2026-11 snapshot from handshake_auth.jsonl").
func AntigravityModelSnapshotMeta() (source, captured string) {
	loadAntigravitySnapshot()
	return antigravitySnapshotInfo.Source, antigravitySnapshotInfo.Captured
}

// loadAntigravitySnapshot decodes the embedded document exactly once.
func loadAntigravitySnapshot() {
	antigravitySnapshotOnce.Do(func() {
		raw, err := antigravityModelsFS.ReadFile("antigravity_models.json")
		if err != nil {
			return
		}
		var doc antigravitySnapshotFile
		if err := json.Unmarshal(raw, &doc); err != nil {
			return
		}
		antigravitySnapshotInfo = antigravitySnapshotMeta{Source: doc.Meta.Source, Captured: doc.Meta.Captured}
		out := make([]ModelInfo, 0, len(doc.Models))
		for _, m := range doc.Models {
			if m.ID == "" {
				continue
			}
			out = append(out, ModelInfo{
				ID: m.ID,
				// A reasoning model is one whose id ends in an effort suffix; the snapshot states the
				// effort it encodes, which is what the picker renders as a capability.
				ReasoningEfforts: append([]string(nil), m.ReasoningEfforts...),
				Visible:          true,
				Provenance:       AntigravitySnapshotProvenance,
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		antigravitySnapshot = out
	})
}
