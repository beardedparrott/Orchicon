package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

// fake project service capturing the Authorization header per call.
type fakeProjects struct {
	apiv1connect.UnimplementedProjectServiceHandler
	auths   []string
	tenant  []string
	streams int
}

func (f *fakeProjects) ListProjects(ctx context.Context, req *connect.Request[v1.ListProjectsRequest]) (*connect.Response[v1.ListProjectsResponse], error) {
	f.auths = append(f.auths, req.Header().Get("Authorization"))
	out := &v1.ListProjectsResponse{}
	out.Projects = append(out.Projects, &v1.Project{Id: "p1", Name: "alpha"})
	return connect.NewResponse(out), nil
}

func (f *fakeProjects) StreamProjectEvents(ctx context.Context, req *connect.Request[v1.StreamProjectEventsRequest], s *connect.ServerStream[v1.StreamProjectEventsResponse]) error {
	f.auths = append(f.auths, req.Header().Get("Authorization"))
	if err := s.Send(&v1.StreamProjectEventsResponse{Event: &v1.ProjectEvent{EventId: "e1"}, Sequence: 1}); err != nil {
		return err
	}
	return nil
}

func TestBearerInterceptorUnaryAndStream(t *testing.T) {
	fake := &fakeProjects{}
	path, handler := apiv1connect.NewProjectServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewWithHTTPClient(Options{BaseURL: srv.URL, Token: "oc_test"}, srv.Client())
	ctx := context.Background()

	resp, err := c.Projects.ListProjects(ctx, connect.NewRequest(&v1.ListProjectsRequest{TenantId: "tnt_x"}))
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(resp.Msg.Projects) != 1 || resp.Msg.Projects[0].Id != "p1" {
		t.Fatalf("unexpected projects: %+v", resp.Msg)
	}

	stream, err := c.Projects.StreamProjectEvents(ctx, connect.NewRequest(&v1.StreamProjectEventsRequest{TenantId: "tnt_x"}))
	if err != nil {
		t.Fatalf("StreamProjectEvents: %v", err)
	}
	if !stream.Receive() {
		t.Fatalf("stream receive failed: %v", stream.Err())
	}
	if stream.Msg().Event.EventId != "e1" {
		t.Fatalf("unexpected event: %+v", stream.Msg())
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream close: %v", err)
	}

	for i, a := range fake.auths {
		if a != "Bearer oc_test" {
			t.Fatalf("call %d: Authorization = %q, want Bearer oc_test", i, a)
		}
	}
}

func TestNoTokenMeansNoHeader(t *testing.T) {
	fake := &fakeProjects{}
	path, handler := apiv1connect.NewProjectServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewWithHTTPClient(Options{BaseURL: srv.URL}, srv.Client())
	if _, err := c.Projects.ListProjects(context.Background(), connect.NewRequest(&v1.ListProjectsRequest{})); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for i, a := range fake.auths {
		if a != "" {
			t.Fatalf("call %d: expected no Authorization header, got %q", i, a)
		}
	}
}

func TestPing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/versionz" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "v9.9.9"})
	}))
	defer srv.Close()

	vr, err := Ping(context.Background(), srv.URL, false)
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if vr.Version != "v9.9.9" {
		t.Fatalf("version = %q, want v9.9.9", vr.Version)
	}
}

func TestPingUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	if _, err := Ping(context.Background(), url, false); err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/local-login" {
			http.NotFound(w, r)
			return
		}
		var lr LoginRequest
		_ = json.NewDecoder(r.Body).Decode(&lr)
		if lr.Username != "me" || lr.Password != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(LoginResponse{AccessToken: "acc", ExpiresIn: 900, IdentityID: "id1", TenantID: "tnt1"})
	}))
	defer srv.Close()

	if _, err := Login(context.Background(), srv.URL, false, "me", "bad"); err == nil {
		t.Fatal("expected error on bad password")
	}
	lr, err := Login(context.Background(), srv.URL, false, "me", "pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if lr.AccessToken != "acc" || lr.IdentityID != "id1" {
		t.Fatalf("login response mismatch: %+v", lr)
	}
}

func TestURLNormalization(t *testing.T) {
	// trailing slash on the base URL must not produce // in the request path
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "v1"})
	}))
	defer srv.Close()
	if _, err := Ping(context.Background(), strings.TrimSuffix(srv.URL, "/")+"/", false); err != nil {
		t.Fatal(err)
	}
	if seen != "/versionz" {
		t.Fatalf("path = %q, want /versionz", seen)
	}
}
