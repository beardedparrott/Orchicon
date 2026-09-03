package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// factsTranscriptMaxParts bounds how many transcript tail parts (newest)
// are scanned for terminal-time facts extraction. Cheap + bounded (far
// smaller than the recovery tail) because facts extraction only needs the
// worker's FINAL assistant block.
const factsTranscriptMaxParts = 200

// extractFactsFromTranscript scans the persisted session-parts tail (which
// both the opencode and native adapters write to execution_session_parts)
// for the worker's assistant `text` part(s) and runs extractFactsLearned
// over them. Mechanical and cheap — no model call. Returns an empty slice
// when there is nothing to extract (empty transcript, no text parts, no
// FACTS-prefixed lines) so callers degrade gracefully.
func extractFactsFromTranscript(parts []db.SessionPart) []string {
	var textParts []string
	for _, p := range parts {
		if p.Kind != db.SessionPartText {
			continue
		}
		var pl struct {
			Text string          `json:"text"`
			Part json.RawMessage `json:"part"`
		}
		if err := json.Unmarshal(p.Payload, &pl); err != nil {
			continue
		}
		text := pl.Text
		// The assistant text often lives under part.text in the raw part JSON
		// (mirroring the transcript renderer in internal/transcript).
		if text == "" && len(pl.Part) > 0 {
			var part struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(pl.Part, &part); err == nil {
				text = part.Text
			}
		}
		if strings.TrimSpace(text) != "" {
			textParts = append(textParts, text)
		}
	}
	var facts []string
	// Process assistant text blocks newest→oldest so the FINAL assistant
	// output (where the ORCHICON WORKER SUMMARY + facts live) wins.
	for i := len(textParts) - 1; i >= 0; i-- {
		facts = append(facts, extractFactsLearned(textParts[i])...)
	}
	return facts
}

// stepAttributedFact renders one facts line carrying (when present) the
// originating step name: `FACTS LEARNED (from <step>): <fact>`, falling back
// to the plain `FACTS LEARNED: <fact>` marker.
func stepAttributedFact(stepName, fact string) string {
	if stepName != "" {
		return "FACTS LEARNED (from " + stepName + "): " + fact
	}
	return "FACTS LEARNED: " + fact
}

// appendFactsToOrchiconFile appends step-attributed facts to
// .orchicon/<run>/facts_learned, deduplicating against both the existing
// file content and anything already appended in this call (exact-string
// equality after whitespace trim). Best-effort: a filesystem error is
// returned for the caller to log. Re-calling on the same file is idempotent
// per fact string (terminal idempotency).
func appendFactsToOrchiconFile(orchDir, stepName string, facts []string) error {
	if len(facts) == 0 {
		return nil
	}
	path := filepath.Join(orchDir, "facts_learned")
	existing := ""
	if b, err := os.ReadFile(path); err == nil {
		existing = string(b)
	}
	// Set of lines that already exist so we never double-append on re-reconcile.
	seen := make(map[string]struct{}, len(facts))
	for _, line := range strings.Split(existing, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			seen[s] = struct{}{}
		}
	}
	var appended []string
	for _, f := range facts {
		if strings.TrimSpace(f) == "" {
			continue
		}
		key := stepAttributedFact(stepName, f)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		appended = append(appended, key)
	}
	if len(appended) == 0 {
		return nil
	}
	var sb strings.Builder
	if existing != "" {
		sb.WriteString(existing)
		if !strings.HasSuffix(existing, "\n") {
			sb.WriteString("\n")
		}
	}
	for _, a := range appended {
		sb.WriteString(a)
		sb.WriteString("\n")
	}
	return os.WriteFile(path, []byte(strings.TrimSpace(sb.String())), 0644)
}
