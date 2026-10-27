package opencode

// session_envelope_test.go — v2 WRAPS SESSION RESPONSES, AND WE MUST UNWRAP THEM.
//
// The reported failure: "create conversation session: opencode session create: empty session id",
// with the models listed fine. opencode 2.x answers the session endpoints with an envelope:
//
//	POST /api/session      -> {"data":{"id":"ses_ed2afaab3ffe9GPiW4pzVdo55d","projectID":"…","title":"…"}}
//	GET  /api/session/{id} -> {"data":{"id":"…", …}}
//
// while GET /api/info answers BARE ({"version":"2.0.26","pid":…}). The client decoded the top level
// for `id`, found nothing, and reported an empty session id — a shape mismatch wearing the costume of
// a server problem.
//
// The fixtures below are the REAL response bodies, captured from the operator's 2.0.26 server, so the
// test is anchored to observed output rather than to a shape invented here.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// realV2SessionCreate is a verbatim v2 response, trimmed to the fields we read.
const realV2SessionCreate = `{"data":{"id":"ses_ed2afaab3ffe9GPiW4pzVdo55d","projectID":"83a434b2df6c6fee704afb31255c959079e613b9","cost":0,"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":1791761601870,"updated":1791761601870},"title":"orchicon-probe","location":{"directory":"/srv/orchicon"}}}`

// realV2Info is a verbatim v2 /api/info body — NOT enveloped, which is the whole reason the decoder
// must tolerate an absent envelope.
const realV2Info = `{"version":"2.0.26","pid":2489580,"urls":["http://127.0.0.1:46035"],"paths":{"tmp":"/tmp/opencode"},"capabilities":{"persistentPty":true}}`

func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCreateSessionUnwrapsTheV2Envelope(t *testing.T) {
	// THE REGRESSION PIN. With the envelope unhandled this returned "empty session id".
	srv := jsonServer(t, realV2SessionCreate)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)

	id, err := c.CreateSession(context.Background(), "x")
	if err != nil {
		t.Fatalf("CreateSession against a real v2 response: %v", err)
	}
	if id != "ses_ed2afaab3ffe9GPiW4pzVdo55d" {
		t.Errorf("session id = %q, want the id from the envelope — an empty id here is the "+
			"\"empty session id\" failure the operator reported", id)
	}
}

func TestSessionExistsUnwrapsTheV2Envelope(t *testing.T) {
	// The same decode path serves this, and it was silently working by accident: it returned true on
	// a SUCCESSFUL decode and never inspected the body, so a shape mismatch was invisible. Now that a
	// decode failure is an error rather than a zero value, it has to actually decode.
	srv := jsonServer(t, realV2SessionCreate)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)

	if !c.SessionExists(context.Background(), "ses_ed2afaab3ffe9GPiW4pzVdo55d") {
		t.Error("SessionExists said no for a session the server returned — the envelope was not unwrapped")
	}
}

func TestAnUnenvelopedResponseStillDecodes(t *testing.T) {
	// /api/info answers bare ON THE SAME SURFACE. A decoder that unwrapped unconditionally would read
	// nothing from it, so the tolerance is not defensive padding — it is required by a measured fact.
	srv := jsonServer(t, realV2Info)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)

	var info struct {
		Version string `json:"version"`
	}
	if err := c.doJSON(context.Background(), http.MethodGet, c.shape.healthPath, nil, &info); err != nil {
		t.Fatalf("an unenveloped v2 response must still decode: %v", err)
	}
	if info.Version != "2.0.26" {
		t.Errorf("version = %q, want 2.0.26 — the bare body was discarded", info.Version)
	}
}

func TestV1ResponsesAreUnchanged(t *testing.T) {
	// v1 has no envelope, and its sessions must keep decoding exactly as before.
	srv := jsonServer(t, `{"id":"ses_1abc"}`)

	c := NewSessionClient(srv.URL, "", "")
	if got := c.APIShapeName(); got != "v1" {
		t.Fatalf("default shape = %q, want v1", got)
	}
	id, err := c.CreateSession(context.Background(), "x")
	if err != nil {
		t.Fatalf("CreateSession on v1 shape: %v", err)
	}
	if id != "ses_1abc" {
		t.Errorf("v1 session id = %q, want ses_1abc", id)
	}
}

func TestAShapeMismatchReportsTheBytes(t *testing.T) {
	// The failure mode that costs the most time is a SILENT empty field. A response we cannot decode
	// at all must therefore name the surface and show what arrived, because on a new API generation
	// "what does it actually return?" is the only useful question and it cannot be guessed.
	srv := jsonServer(t, `{"unexpected":"shape"}`)

	c := NewSessionClient(srv.URL, "", "")
	c.setAPIShape(shapeV2)

	_, err := c.CreateSession(context.Background(), "x")
	if err == nil {
		t.Fatal("expected an error for a body with no usable session id")
	}
	// Either the decode reports the bytes, or the empty-id check fires — both name the problem. What
	// must NOT happen is a bare, context-free error.
	msg := err.Error()
	if !strings.Contains(msg, "decode") && !strings.Contains(msg, "empty session id") {
		t.Errorf("the error must name what went wrong, got: %v", err)
	}
}
