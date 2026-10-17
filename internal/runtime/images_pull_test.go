package runtime

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPullSkipsWhenImageIsPresent pins fetch IDEMPOTENCY (AC 12): an image that
// is already present locally is reported skipped with exit 0 and NO `pull`
// command runs at all. That is what makes the Fetch action safe to press twice
// and what keeps a fetch from ever re-downloading multiple GB.
func TestPullSkipsWhenImageIsPresent(t *testing.T) {
	const ref = "ghcr.io/beardedparrott/orchicon-runtime:gui"
	d := &Daemon{Image: ref}
	pulled := false
	d.engineFn = func(args ...string) (string, error) {
		if len(args) == 0 {
			t.Fatalf("unexpected empty engine call")
		}
		if args[0] == "pull" {
			pulled = true
			return "", nil
		}
		// `image inspect` succeeding IS presence; the labels are irrelevant here.
		return "|7", nil
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/pull?ref="+ref, nil)
	d.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("pull of a present image must succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	if pulled {
		t.Fatal("a present image must NOT be pulled — that is the idempotency guarantee")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"skipped":true`) {
		t.Errorf("the exit event must carry skipped=true so the RPC can report a no-op fetch; body=%s", body)
	}
	// exit_code is `omitempty`, so a zero exit is the ABSENCE of a non-zero code.
	if !strings.Contains(body, `"event":"exit"`) || strings.Contains(body, `"exit_code":`) {
		t.Errorf("a skipped fetch must still end with a zero exit event; body=%s", body)
	}
	if !strings.Contains(body, "already present") {
		t.Errorf("the operator must be told why nothing was downloaded; body=%s", body)
	}
}

// TestPullRejectsBadRef keeps the fetch surface closed: the daemon never runs
// an engine command for a ref that is not a plain image tag.
func TestPullRejectsBadRef(t *testing.T) {
	d := &Daemon{Image: "orchicon-runtime:base"}
	d.engineFn = func(args ...string) (string, error) {
		t.Fatalf("a rejected ref must not reach the engine, got %v", args)
		return "", errors.New("unreachable")
	}
	rec := httptest.NewRecorder()
	d.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/pull?ref=$(whoami)", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a malformed ref, got %d", rec.Code)
	}
}
