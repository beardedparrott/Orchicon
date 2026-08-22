package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/config"
)

// testDeps builds a Dependencies with a DB-free auth handler and nil pool —
// enough to exercise the api.Mount wiring. No RPC handler is reached by
// these tests: auth rejects the requests before dispatch.
func testDeps() Dependencies {
	log := slog.New(slog.DiscardHandler)
	cfg := config.Config{
		Mode:               config.ModeLocal,
		DeploymentTenantID: "tnt_dev",
		Auth: config.AuthConfig{
			Issuer:     "local",
			SigningKey: "test-signing-key-not-for-production",
			AccessTTL:  15 * time.Minute,
			RefreshTTL: 24 * time.Hour,
			EmbeddedOP: false,
		},
	}
	return Dependencies{
		Log:         log,
		AuthHandler: auth.NewHandler(cfg, nil, log),
	}
}

// TestMountRequiresCredential pins AC1/AC2 at the api.Mount level: a
// request with no Authorization header is 401. There is no tenant-only
// fallback — Mount always wraps with ResolveAuth, so no credential-less
// request can reach tenant-scoped data.
func TestMountRequiresCredential(t *testing.T) {
	mux := http.NewServeMux()
	handler := Mount(mux, testDeps())
	req := httptest.NewRequest(http.MethodPost, "/orchicon.api.v1.ProjectService/ListProjects", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (missing credentials)", rec.Code)
	}
}

// TestMountTenantHeaderIsNotIdentity pins AC3 at the api.Mount level: the
// x-orchicon-tenant-id header alone (no credential) must never substitute
// for a resolved identity.
func TestMountTenantHeaderIsNotIdentity(t *testing.T) {
	mux := http.NewServeMux()
	handler := Mount(mux, testDeps())
	req := httptest.NewRequest(http.MethodPost, "/orchicon.api.v1.ProjectService/ListProjects", nil)
	req.Header.Set("x-orchicon-tenant-id", "tnt_dev")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (header is not a credential)", rec.Code)
	}
}

// TestMountPublicPathsRemainPublic sanity-checks that removing the
// tenant-only fallback did not break the public health endpoint.
func TestMountPublicPathsRemainPublic(t *testing.T) {
	mux := http.NewServeMux()
	handler := Mount(mux, testDeps())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
