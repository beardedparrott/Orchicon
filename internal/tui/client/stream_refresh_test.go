package client

// stream_refresh_test.go — A LIVE STREAM RECOVERS FROM AN EXPIRED TOKEN.
//
// THE BUG THIS PINS. SessionClient.reDial was READ at refresh.go's streaming 401 path and
// assigned NOWHERE, so it was nil for every real client. A server-stream is opened once:
// connect has no interceptor left to run when the open is rejected, so the only recovery
// is the one reDial exists for — and the wrapper found nil, refreshed the token (burning
// it), and returned the original 401 anyway:
//
//	if s.retried || s.interceptor == nil || s.interceptor.sc == nil {
//		return err
//	}
//	if rerr := s.interceptor.refreshOnce(s.ctx); rerr != nil {
//		return err
//	}
//	if s.interceptor.sc.reDial == nil {
//		return err          // ← every real client took this line
//	}
//
// The consequence is the operator's report: "Every so often it just loses its connection
// and trying to use /connect doesn't work. You have to exit the TUI and restart it."
// A stream whose token expired mid-session could not refresh its way back, so it went to
// StatusError and the shell declared the plane unreachable — while RESTARTING worked,
// because a fresh client set starts with a valid token.
//
// NOTHING COVERED THIS PATH. There was no streaming-refresh test at all, which is exactly
// how a documented seam stayed unwired: the unary path has a test, so the field looked
// used.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

// rotatingProjects rejects any bearer but the CURRENT one, so the first stream open is
// refused with UNAUTHENTICATED and the retry succeeds only if it carries the refreshed
// token. It records every Authorization header it saw, so the test can prove the re-dial
// used the NEW credential rather than repeating the stale one.
type rotatingProjects struct {
	apiv1connect.UnimplementedProjectServiceHandler

	mu     sync.Mutex
	token  string   // the only bearer this server accepts
	auths  []string // every Authorization header, in order
	stream int      // how many stream opens were attempted
}

func (f *rotatingProjects) setToken(t string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = t
}

func (f *rotatingProjects) seen() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...), f.stream
}

func (f *rotatingProjects) StreamProjectEvents(
	_ context.Context,
	req *connect.Request[v1.StreamProjectEventsRequest],
	s *connect.ServerStream[v1.StreamProjectEventsResponse],
) error {
	auth := req.Header().Get("Authorization")
	f.mu.Lock()
	f.auths = append(f.auths, auth)
	f.stream++
	want := "Bearer " + f.token
	f.mu.Unlock()

	if auth != want {
		return connect.NewError(connect.CodeUnauthenticated, nil)
	}
	if err := s.Send(&v1.StreamProjectEventsResponse{Event: &v1.ProjectEvent{EventId: "e1"}, Sequence: 1}); err != nil {
		return err
	}
	return nil
}

// A STREAM OPENED WITH A STALE TOKEN RE-DIALS WITH THE REFRESHED ONE.
//
// This is the whole point of reDial: the stream must come back, and on the connection it
// must carry the NEW bearer. Asserting only "an error did not surface" would pass on a
// re-dial that repeated the stale header, so the header is asserted too.
func TestAStreamReDialsWithTheRefreshedToken(t *testing.T) {
	projects := &rotatingProjects{}
	projects.setToken("oc_fresh")

	// ONE server carries both: the stream RPC and /auth/refresh must be reachable at the
	// same base URL, because that is the shape the client dials.
	path, handler := apiv1connect.NewProjectServiceHandler(projects)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	mux.HandleFunc("/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RefreshResponse{AccessToken: "oc_fresh"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewWithHTTPClient(Options{
		BaseURL:      srv.URL,
		Token:        "oc_stale",
		RefreshToken: "rt_1",
	}, srv.Client())

	ctx := context.Background()
	st, err := c.Projects.StreamProjectEvents(ctx, connect.NewRequest(&v1.StreamProjectEventsRequest{TenantId: "tnt_x"}))
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	// The FIRST Receive is where a rejected open surfaces, and where the recovery runs.
	if !st.Receive() {
		serr := st.Err()
		auths, streams := projects.seen()
		t.Fatalf("the stream did not recover from an expired token: %v\n"+
			"  stream opens: %d\n  bearers seen: %v\n"+
			"  (before the fix, reDial was nil for every real client, so this is where the "+
			"session just died)", serr, streams, auths)
	}
	if st.Msg().GetEvent().GetEventId() != "e1" {
		t.Fatalf("unexpected event: %+v", st.Msg())
	}
	_ = st.Close()

	auths, streams := projects.seen()
	if streams < 2 {
		t.Fatalf("the rejected open was not re-dialed at all (%d stream open(s))", streams)
	}
	// The LAST attempt must carry the refreshed credential. A re-dial that repeated the
	// stale bearer would be an infinite 401 loop dressed up as a recovery.
	last := auths[len(auths)-1]
	if last != "Bearer oc_fresh" {
		t.Fatalf("the re-dial sent %q, want the REFRESHED credential (Bearer oc_fresh)\n"+
			"  every bearer seen, in order: %v", last, auths)
	}
	if auths[0] == last {
		t.Fatalf("the stream never changed credential — both attempts sent %q", last)
	}
}

// A STALE TOKEN ALONE DOES NOT SAVE YOU: when the refresh itself fails, the ORIGINAL 401
// is what surfaces (it is the meaningful failure — the session is over, not the stream),
// and the shell must be able to see it and route the operator to /connect.
func TestAFailedRefreshSurfacesTheOriginalUnauthenticated(t *testing.T) {
	projects := &rotatingProjects{}
	projects.setToken("oc_fresh") // nothing the client can ever present

	mux := http.NewServeMux()
	path, handler := apiv1connect.NewProjectServiceHandler(projects)
	mux.Handle(path, handler)
	mux.HandleFunc("/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid or expired refresh token", http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewWithHTTPClient(Options{
		BaseURL:      srv.URL,
		Token:        "oc_stale",
		RefreshToken: "rt_dead",
	}, srv.Client())

	st, err := c.Projects.StreamProjectEvents(context.Background(), connect.NewRequest(&v1.StreamProjectEventsRequest{TenantId: "tnt_x"}))
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if st.Receive() {
		t.Fatal("a stream whose refresh failed must NOT report success")
	}
	serr := st.Err()
	if serr == nil {
		t.Fatal("a failed stream must report an error")
	}
	if !strings.Contains(strings.ToLower(serr.Error()), "unauthenticated") {
		t.Fatalf("the surfaced error is %q, want the ORIGINAL unauthenticated — the shell keys "+
			"the /connect prompt off it, and a wrapped refresh error would hide the cause", serr)
	}
}

// API-KEY MODE IS UNTOUCHED: there is no session, so there is nothing to wire and a 401 is
// a genuine authorization failure that must surface as-is (never refreshed, never retried).
func TestAPIKeyModeStreamDoesNotTryToRefresh(t *testing.T) {
	projects := &rotatingProjects{}
	projects.setToken("oc_something_else")

	mux := http.NewServeMux()
	path, handler := apiv1connect.NewProjectServiceHandler(projects)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewWithHTTPClient(Options{BaseURL: srv.URL, Token: "oc_apikey"}, srv.Client())
	if c.Session != nil {
		t.Fatal("api-key mode must have no session (there is nothing to refresh)")
	}

	st, err := c.Projects.StreamProjectEvents(context.Background(), connect.NewRequest(&v1.StreamProjectEventsRequest{TenantId: "tnt_x"}))
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if st.Receive() {
		t.Fatal("a rejected api-key stream must not report success")
	}
	_, streams := projects.seen()
	if streams != 1 {
		t.Fatalf("api-key mode must NOT re-dial (it has no fresh credential to offer), got %d opens", streams)
	}
}

// EVERY STREAM THE TUI SUBSCRIBES TO HAS A RE-DIAL.
//
// wireStreamReDial is an explicit mapping (procedure → client call) rather than a
// reflective one, so a NEW stream RPC would silently fall through to "no streaming client
// for …" and its 401 recovery would be broken in exactly the way this file was written to
// prevent. The list below is what the TUI actually opens, derived from the stream
// constructors in subs.go and the chat controller, so adding a subscription without adding
// its re-dial fails HERE rather than in the field.
func TestEveryStreamTheTUIDialsHasAReDial(t *testing.T) {
	// A client set with a session, so the wiring runs.
	c := NewWithHTTPClient(Options{BaseURL: "http://x.invalid", Token: "oc_t", RefreshToken: "rt"}, http.DefaultClient)
	if c.Session == nil || c.Session.reDial == nil {
		t.Fatal("a refreshable client set must have its stream re-dial wired")
	}

	// The procedures the TUI opens, each with the connect.Spec the wrapper would be
	// handed. A spec the wrapper can pass through is one whose procedure resolves.
	opened := map[string]connect.Spec{
		"project events":   {Procedure: apiv1connect.ProjectServiceStreamProjectEventsProcedure},
		"execution events": {Procedure: apiv1connect.ExecutionServiceStreamExecutionEventsProcedure},
		"workflow events":  {Procedure: apiv1connect.WorkflowServiceStreamWorkflowEventsProcedure},
		"recovery events":  {Procedure: apiv1connect.RecoveryServiceStreamRecoveryEventsProcedure},
		"telemetry":        {Procedure: apiv1connect.TelemetryServiceStreamTelemetryProcedure},
		"file edits":       {Procedure: apiv1connect.FileEditServiceStreamFileEditsProcedure},
	}
	for name, spec := range opened {
		conn, err := c.Session.reDial(context.Background(), spec)
		if err != nil && strings.Contains(err.Error(), "no streaming client for") {
			t.Errorf("%s (%s) has NO re-dial: a 401 on this stream can never recover", name, spec.Procedure)
			continue
		}
		// Any other outcome is fine here: the point is that the procedure RESOLVED to a
		// client. (The dial itself fails against x.invalid, which is expected.)
		if conn != nil {
			_ = conn.CloseRequest()
		}
	}
}
