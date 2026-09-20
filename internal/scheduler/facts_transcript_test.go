package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

func partText(seq int64, text, partText string) db.SessionPart {
	part := map[string]any{"text": partText}
	if partText == "" {
		part = map[string]any{"text": ""}
	}
	payload, _ := json.Marshal(map[string]any{"text": text, "part": part})
	return db.SessionPart{Seq: seq, Kind: db.SessionPartText, Payload: payload}
}

// TestExtractFactsFromTranscript covers the mechanical transcript extractor:
// empty/absent transcript degrades to nothing; the final assistant block's
// FACTS LEARNED lines win over earlier recon turns.
func TestExtractFactsFromTranscript(t *testing.T) {
	if got := extractFactsFromTranscript(nil); len(got) != 0 {
		t.Fatalf("nil transcript yielded facts: %v", got)
	}
	parts := []db.SessionPart{
		partText(1, "recon no marker", "recon no marker"),
		partText(2, "more recon\nFACTS LEARNED: first fact.\n", "more recon\nFACTS LEARNED: first fact.\n"),
		partText(3, "final\nFACTS LEARNED: last fact.\n", "final\nFACTS LEARNED: last fact.\n"),
	}
	facts := extractFactsFromTranscript(parts)
	if len(facts) != 2 {
		t.Fatalf("want 2 facts, got %d: %v", len(facts), facts)
	}
	if facts[0] != "last fact." {
		t.Errorf("facts[0]=%q, want final fact to win ordering", facts[0])
	}
}

// TestAppendFactsToOrchiconFile covers creation, step attribution, exact
// dedup against disk + same-call, and terminal idempotency (re-call no-op).
func TestAppendFactsToOrchiconFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "facts_learned")
	if err := appendFactsToOrchiconFile(dir, "Senior Software Engineer", []string{"a fact"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "FACTS LEARNED (from Senior Software Engineer): a fact" {
		t.Fatalf("first write wrong: %q", string(b))
	}
	// New + duplicate facts: only the new one lands.
	if err := appendFactsToOrchiconFile(dir, "Senior Software Engineer", []string{"a fact", "another fact"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := "FACTS LEARNED (from Senior Software Engineer): a fact\nFACTS LEARNED (from Senior Software Engineer): another fact"
	if string(b) != want {
		t.Fatalf("dedup wrong:\n%q\nwant:\n%q", string(b), want)
	}
	// Terminal idempotency.
	if err := appendFactsToOrchiconFile(dir, "Senior Software Engineer", []string{"a fact", "another fact"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if string(b) != want {
		t.Fatalf("idempotency violated:\n%q\nwant:\n%q", string(b), want)
	}
	// Missing step name falls back to plain marker.
	if err := appendFactsToOrchiconFile(dir, "", []string{"plain fact"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "FACTS LEARNED: plain fact") {
		t.Fatalf("plain fallback missing:\n%s", string(b))
	}
}
