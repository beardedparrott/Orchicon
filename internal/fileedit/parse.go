package fileedit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// EngineFileEdit is one entry of the `file_edits` array the worktree engine
// (batch_write / write / edit) embeds in its structured JSON output. The
// engine holds the original on-disk content in memory while it applies each
// op, so the diff it emits is computed from real file state with zero extra
// I/O and no race window — the ground truth the ledger stores verbatim.
type EngineFileEdit struct {
	Path          string `json:"path"`
	Kind          string `json:"kind"` // create | modify | delete
	UnifiedDiff   string `json:"unified_diff,omitempty"`
	ExistedBefore bool   `json:"existed_before"`
	ExistedAfter  bool   `json:"existed_after"`
	SizeBefore    int64  `json:"size_before"`
	SizeAfter     int64  `json:"size_after"`
	SHA256Before  string `json:"sha256_before,omitempty"`
	SHA256After   string `json:"sha256_after,omitempty"`
	IsBinary      bool   `json:"is_binary,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
}

// engineOutput is the subset of the worktree tools' JSON output the ledger
// parses. Parsing STRUCTURED OUTPUT the engine produced as data is not
// tool-output text scraping — the plane consumes it as a typed record.
type engineOutput struct {
	FileEdits []EngineFileEdit `json:"file_edits"`
}

// ParseEngineOutput extracts the file_edits array from a batch_write/write/
// edit tool output. Tolerant by design: a missing/empty/malformed field
// yields zero entries and no error (non-file tools and legacy outputs are
// the norm here, never a failure).
func ParseEngineOutput(toolOutput string) ([]EngineFileEdit, error) {
	if toolOutput == "" || !strings.Contains(toolOutput, "\"file_edits\"") {
		return nil, nil
	}
	// The engine emits the JSON object AFTER a human summary line
	// ("batch_write: applied N write(s): …\n{…}"), so the payload is rarely
	// the whole string. Scan for the first '{' whose decode succeeds; the
	// decoder reads only the first JSON value, tolerating trailing text.
	// Any parse problem yields zero entries and no error (tolerant).
	for i := strings.IndexByte(toolOutput, '{'); i >= 0 && i < len(toolOutput); i = i + 1 + strings.IndexByte(toolOutput[i+1:], '{') {
		var out engineOutput
		dec := json.NewDecoder(strings.NewReader(toolOutput[i:]))
		if err := dec.Decode(&out); err == nil && len(out.FileEdits) > 0 {
			return out.FileEdits, nil
		}
	}
	return nil, nil
}

// Entry is a ledger-ready edit: everything AppendFileEditLedger needs to
// persist one row, already normalized (path slash-separated, sizes/shas
// computed from the snapshots).
type Entry struct {
	Path        string
	Kind        string
	UnifiedDiff string
	BeforeSize  int64
	AfterSize   int64
	BeforeSHA   string
	AfterSHA    string
	Tool        string
	IsBinary    bool
	Truncated   bool
}

// EntryFromEngine converts one engine file_edits entry into a ledger Entry.
func EntryFromEngine(e EngineFileEdit, tool string) Entry {
	return Entry{
		Path:        NormalizePath(e.Path),
		Kind:        e.Kind,
		UnifiedDiff: e.UnifiedDiff,
		BeforeSize:  e.SizeBefore,
		AfterSize:   e.SizeAfter,
		BeforeSHA:   e.SHA256Before,
		AfterSHA:    e.SHA256After,
		Tool:        tool,
		IsBinary:    e.IsBinary,
		Truncated:   e.Truncated,
	}
}

// EntryFromSnapshots computes a ledger Entry from a raw before/after snapshot
// pair via the shared diff engine (the path the opencode built-in write/edit
// tools and the git reconciler take). before/after nil = absent/deleted.
func EntryFromSnapshots(before, after []byte, rawPath, tool string) Entry {
	res := ComputeUnifiedDiff(before, after, NormalizePath(rawPath))
	return Entry{
		Path:        NormalizePath(rawPath),
		Kind:        res.Kind,
		UnifiedDiff: res.UnifiedDiff,
		BeforeSize:  int64(len(before)),
		AfterSize:   int64(len(after)),
		BeforeSHA:   SHA256Hex(before),
		AfterSHA:    SHA256Hex(after),
		Tool:        tool,
		IsBinary:    res.Binary,
		Truncated:   res.Truncated,
	}
}

// NormalizePath makes a ledger path stable and comparable across tools:
// slash-separated, no leading "./", no leading slash (worktree-relative).
func NormalizePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return p
}

// SHA256Hex is the content fingerprint stored on every ledger row (so a
// renderer can cheaply detect "same content re-saved").
func SHA256Hex(b []byte) string {
	if b == nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// String renders an entry for logs.
func (e Entry) String() string {
	return fmt.Sprintf("%s %s (%s) diff=%dB", e.Kind, e.Path, e.Tool, len(e.UnifiedDiff))
}
