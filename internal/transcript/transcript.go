// Package transcript renders durable execution session transcripts (execution_session_parts) into
// readable text.
package transcript

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// RenderParts renders session parts into readable chronological text.
func RenderParts(parts []db.SessionPart, maxContext, perPartCap int, truncMarker string) string {
	var sb strings.Builder
	trunc := func(text string) string {
		if len(text) > perPartCap {
			return text[:perPartCap] + "\n…(truncated)"
		}
		return text
	}
	for _, p := range parts {
		var pl struct {
			Text   string          `json:"text"`
			Source string          `json:"source"`
			Part   json.RawMessage `json:"part"`
		}
		_ = json.Unmarshal(p.Payload, &pl)
		switch p.Kind {
		case db.SessionPartUserMessage:
			sb.WriteString("USER (" + pl.Source + "): " + trunc(pl.Text) + "\n\n")
		case db.SessionPartText:
			// The assistant text lives under part.text in the raw part JSON.
			var part struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(pl.Part, &part)
			if part.Text != "" {
				sb.WriteString("ASSISTANT: " + trunc(part.Text) + "\n\n")
			}
		case db.SessionPartToolUse:
			var part struct {
				Tool string `json:"tool"`
			}
			_ = json.Unmarshal(pl.Part, &part)
			if part.Tool != "" {
				sb.WriteString("TOOL CALL: " + part.Tool + "\n\n")
			}
		case db.SessionPartReasoning, db.SessionPartStepStart, db.SessionPartStepFinish, db.SessionPartSessionInfo, db.SessionPartSystemPrompt:
			// skip — verbose, and the assistant text carries the outcome.
		case db.SessionPartError:
			sb.WriteString("ERROR\n\n")
		}
		if sb.Len() >= maxContext {
			sb.WriteString(truncMarker)
			break
		}
	}
	return sb.String()
}

// RenderTail renders the session parts into a bounded, chronological transcript text suitable for
// seeding a recovery-resumed worker (.orchicon/worker.recovery).
func RenderTail(parts []db.SessionPart, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 24 * 1024
	}
	sorted := make([]db.SessionPart, len(parts))
	copy(sorted, parts)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	return RenderParts(sorted, maxBytes, 1200, "\n…(transcript truncated)\n")
}
