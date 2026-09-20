package execution

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestRenderSessionContextExtractsAdapterKind pins the follow-up seed's
// structural read of the execution transcript's session_info part: the adapter
// identity is extracted ALONGSIDE the opencode-shaped session_id/serve_url, and
// a legacy row (no adapter_kind) still resolves — with an EMPTY kind, which is
// what keeps the adapters' historical best-effort continuity path alive.
func TestRenderSessionContextExtractsAdapterKind(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantID   string
		wantURL  string
		wantKind string
	}{
		{
			name:     "opencode-tagged row",
			payload:  `{"session_id":"ses_1","serve_url":"http://127.0.0.1:4242","adapter_kind":"opencode"}`,
			wantID:   "ses_1",
			wantURL:  "http://127.0.0.1:4242",
			wantKind: "opencode",
		},
		{
			// A native run happens in-process: the part carries the adapter kind
			// and NO serve_url key at all (absent, not an empty string).
			name:     "native row",
			payload:  `{"session_id":"exec_1","adapter_kind":"orchicon"}`,
			wantID:   "exec_1",
			wantKind: "orchicon",
		},
		{
			name:    "legacy row keeps resolving",
			payload: `{"session_id":"ses_old","serve_url":"http://127.0.0.1:1"}`,
			wantID:  "ses_old",
			wantURL: "http://127.0.0.1:1",
		},
		{
			name:    "row without a session_info identity",
			payload: `{"text":"hi"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := []db.SessionPart{{Kind: db.SessionPartSessionInfo, Seq: 1, Payload: []byte(tc.payload)}}
			_, sessionID, serveURL, _, adapterKind := renderSessionContext(parts)
			if sessionID != tc.wantID {
				t.Errorf("sessionID = %q, want %q", sessionID, tc.wantID)
			}
			if serveURL != tc.wantURL {
				t.Errorf("serveURL = %q, want %q", serveURL, tc.wantURL)
			}
			if adapterKind != tc.wantKind {
				t.Errorf("adapterKind = %q, want %q", adapterKind, tc.wantKind)
			}
		})
	}
}

// TestRenderSessionContextLastNonEmptyAdapterKindWins pins the loop's
// last-write-wins semantics for the identity fields: a follow-up that recorded a
// FRESH session (adapter-tagged) after the original run's part must win, so the
// next continuation resolves the most recent transport identity.
func TestRenderSessionContextLastNonEmptyAdapterKindWins(t *testing.T) {
	parts := []db.SessionPart{
		{Kind: db.SessionPartSessionInfo, Seq: 1, Payload: []byte(`{"session_id":"ses_old","serve_url":"http://127.0.0.1:1"}`)},
		{Kind: db.SessionPartSessionInfo, Seq: 9, Payload: []byte(`{"session_id":"exec_2","adapter_kind":"orchicon"}`)},
	}
	_, sessionID, serveURL, _, adapterKind := renderSessionContext(parts)
	if sessionID != "exec_2" || adapterKind != "orchicon" {
		t.Errorf("identity = (%q, %q), want (exec_2, orchicon)", sessionID, adapterKind)
	}
	// serve_url is only overwritten by a non-empty value, so the legacy URL
	// survives — it is display/diagnostic, never the transport resolution.
	if serveURL != "http://127.0.0.1:1" {
		t.Errorf("serveURL = %q, want the recorded legacy URL", serveURL)
	}
}
