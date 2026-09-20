// Package testfixtures holds the shared diff-pipeline test vectors. ONE JSON
// file per vector drives BOTH the Go pipeline tests (internal/fileedit) and
// the TS renderer tests (frontend/src/lib/fileedit) — the same fixture must
// produce byte-identical diff text in both implementations.
package testfixtures

import (
	"embed"
	"encoding/json"
	"fmt"
)

// FileEditFS embeds the fileedit vectors.
//
//go:embed fileedit/*.json
var FileEditFS embed.FS

// FileEditVector is one shared test vector: a before/after file snapshot pair
// plus the expected ledger-entry fields.
type FileEditVector struct {
	Name                 string  `json:"name"`
	Path                 string  `json:"path"`
	Before               *string `json:"before"` // null = file did not exist
	After                *string `json:"after"`  // null = file deleted
	ExpectedKind         string  `json:"expected_kind"`
	ExpectedUnifiedDiff  *string `json:"expected_unified_diff"` // null = assert only the byte cap + truncation flag
	ExpectedBinary       bool    `json:"expected_binary"`
	ExpectedTruncated    bool    `json:"expected_truncated"`
	ExpectedSkipEntry    bool    `json:"expected_skip_entry"`
	ExpectedMaxDiffBytes int     `json:"expected_max_diff_bytes"`
}

// LoadFileEditVectors decodes every embedded fileedit/*.json vector.
func LoadFileEditVectors() ([]FileEditVector, error) {
	entries, err := FileEditFS.ReadDir("fileedit")
	if err != nil {
		return nil, err
	}
	var out []FileEditVector
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := FileEditFS.ReadFile("fileedit/" + e.Name())
		if err != nil {
			return nil, err
		}
		var v FileEditVector
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		out = append(out, v)
	}
	return out, nil
}
