package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

// new401Server builds a fake ProjectService whose handler rejects
// "Bearer old-token" with 401 (the expired-access-token case) and accepts
// everything else, plus a /auth/refresh exchange endpoint.
func new401Server(t *testing.T, refreshCount *atomic.Int32, refreshOK bool) *httptest.Server {
	t.Helper()
	fake := &fakeProjects{}
	path, handler := apiv1connect.NewProjectServiceHandler(fake)
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer old-token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthenticated"}`))
			return
		}
		handler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		refreshCount.Add(1)
		if !refreshOK {
			http.Error(w, "invalid or expired refresh token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-token","token_type":"Bearer","expires_in":900}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRefreshOn401UnaryAndRetry pins the password-mode auto-refresh: the
// first RPC 401s (access token expired), the interceptor POSTs
// /auth/refresh with the stored token, swaps in the fresh access token,
// and RETRIES — the caller never sees the 401.
func TestRefreshOn401UnaryAndRetry(t *testing.T) {
	var refreshes atomic.Int32
	srv := new401Server(t, &refreshes, true)

	c := NewWithHTTPClient(Options{BaseURL: srv.URL, Token: "old-token", RefreshToken: "refresh-tok", Timeout: 5 * time.Second}, srv.Client())
	if c.Session == nil {
		t.Fatal("refreshable options must build a Session")
	}
	holder := c.sessionHolder()
	if holder == nil {
		t.Fatal("sessionHolder must not be nil with a Session present")
	}

	resp, err := c.Projects.ListProjects(context.Background(), connect.NewRequest(&v1.ListProjectsRequest{TenantId: "tnt_x"}))
	if err != nil {
		t.Fatalf("ListProjects after refresh: %v", err)
	}
	if resp.Msg.GetProjects()[0].GetId() != "p1" {
		t.Fatalf("unexpected projects: %+v", resp.Msg)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshes.Load())
	}
	if holder.get() != "new-token" {
		t.Fatalf("token holder = %q, want new-token", holder.get())
	}
}

// TestRefreshFailureSurfaces401 pins the other half: when refresh itself
// fails (expired refresh token) the ORIGINAL 401 surfaces — the
// auth-expired state fires and drives the in-place /connect overlay.
func TestRefreshFailureSurfaces401(t *testing.T) {
	var refreshes atomic.Int32
	srv := new401Server(t, &refreshes, false)

	c := NewWithHTTPClient(Options{BaseURL: srv.URL, Token: "old-token", RefreshToken: "refresh-tok", Timeout: 5 * time.Second}, srv.Client())
	_, err := c.Projects.ListProjects(context.Background(), connect.NewRequest(&v1.ListProjectsRequest{TenantId: "tnt_x"}))
	if err == nil {
		t.Fatal("want the 401 to surface when refresh fails")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("err code = %v, want unauthenticated", connect.CodeOf(err))
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshes.Load())
	}
}

// TestAPIKeyModeNeverRefreshes pins api-key inertness: no refresh token
// stored → no /auth/refresh call, no Session, and the stale credential
// still surfaces its 401 as-is (no retry loop).
func TestAPIKeyModeNeverRefreshes(t *testing.T) {
	fake := &fakeProjects{}
	path, _ := apiv1connect.NewProjectServiceHandler(fake)
	mux := http.NewServeMux()
	var refreshes atomic.Int32
	mux.HandleFunc("/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"should-not-happen"}`))
	})
	// 401 the stale api key: api-key mode must surface it as-is.
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthenticated"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewWithHTTPClient(Options{BaseURL: srv.URL, Token: "oc_key", Timeout: 5 * time.Second}, srv.Client())
	if c.Session != nil {
		t.Fatal("api-key mode must not build a Session")
	}
	_, err := c.Projects.ListProjects(context.Background(), connect.NewRequest(&v1.ListProjectsRequest{TenantId: "tnt_x"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("err = %v, want unauthenticated surfaced as-is", err)
	}
	if refreshes.Load() != 0 {
		t.Fatalf("refresh calls = %d, want 0 (api-key mode is inert)", refreshes.Load())
	}
}

// TestLoginCapturesRefreshCookie pins the login-side half: POST
// /auth/local-login returns the access token in the body and the refresh
// token ONLY as the HttpOnly orchicon_refresh Set-Cookie — Login must
// surface it for the profile to store.
func TestLoginCapturesRefreshCookie(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/local-login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "orchicon_refresh", Value: "refresh-tok-24h", Path: "/", HttpOnly: true})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"acc-1","token_type":"Bearer","expires_in":900,"identity_id":"idn_1","tenant_id":"tnt_dev","is_admin":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	lr, err := Login(context.Background(), srv.URL, false, "op", "pw")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if lr.AccessToken != "acc-1" {
		t.Fatalf("access token = %q", lr.AccessToken)
	}
	if lr.RefreshToken != "refresh-tok-24h" {
		t.Fatalf("refresh token = %q, want the orchicon_refresh cookie value", lr.RefreshToken)
	}
}

// TestRefreshTokenRoundTrip pins the POST /auth/refresh body contract:
// {"refresh_token": …} in, tokenResponse out. The refresh token is
// replayed in the body (readRefreshToken accepts cookie OR body;
// non-browser clients have no cookie jar).
func TestRefreshTokenRoundTrip(t *testing.T) {
	mux := http.NewServeMux()
	var gotBody atomic.Value
	mux.HandleFunc("/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		gotBody.Store(string(buf))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh","token_type":"Bearer","expires_in":900}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rr, err := RefreshToken(context.Background(), srv.URL, false, "r-tok")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if rr.AccessToken != "fresh" {
		t.Fatalf("access token = %q", rr.AccessToken)
	}
	if body, _ := gotBody.Load().(string); !strings.Contains(body, `"refresh_token":"r-tok"`) {
		t.Fatalf("refresh body = %q, want the refresh_token JSON shape", body)
	}
}

// TestLoginNoCookieLeavesRefreshEmpty pins the api-key/probe servers
// without the refresh cookie: RefreshToken stays "" (inert).
func TestLoginNoCookieLeavesRefreshEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/local-login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"acc-2"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	lr, err := Login(context.Background(), srv.URL, false, "op", "pw")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if lr.RefreshToken != "" {
		t.Fatalf("refresh token = %q, want empty without the cookie", lr.RefreshToken)
	}
}
