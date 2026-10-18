package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// AC6: a version mismatch is SURFACED, not silently rendered. The rule is
// coherence (both tags known and different), not semver ordering.
func TestBuildInfoMismatchRule(t *testing.T) {
	cases := []struct {
		app, plane string
		want       bool
	}{
		{"v1.2.0", "v1.3.0", true},
		{"v1.2.0", "v1.2.0", false},
		{"dev", "v1.2.0", true},             // a dev-built app against a tagged plane
		{"v1.2.0", PlaneUnreachable, false}, // a plane that is down is a different failure
		{"", "v1.2.0", false},
		{"v1.2.0", "", false},
	}
	for _, c := range cases {
		got := BuildInfo("http://127.0.0.1:8080", c.app, c.plane)
		if got.VersionMismatch != c.want {
			t.Errorf("BuildInfo(app=%q, plane=%q).VersionMismatch = %v, want %v", c.app, c.plane, got.VersionMismatch, c.want)
		}
		if got.AppTag != c.app || got.PlaneTag != c.plane {
			t.Errorf("tags not carried through: %+v", got)
		}
	}
}

// AC9: the instance comes from the plane URL through the shared port table.
func TestBuildInfoResolvesInstance(t *testing.T) {
	for url, want := range map[string]string{
		"http://127.0.0.1:8080": "dev",
		"http://127.0.0.1:8091": "prod",
		"http://127.0.0.1:8092": "test",
		"http://127.0.0.1:9999": UnknownInstance,
	} {
		if got := BuildInfo(url, "v", "v").Instance; got != want {
			t.Errorf("BuildInfo(%q).Instance = %q, want %q", url, got, want)
		}
	}
}

// The plane's tag is read from /versionz, the same endpoint the operator's
// curl uses.
func TestFetchPlaneTag(t *testing.T) {
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/versionz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"version":"v9.9.9"}`))
	}))
	defer plane.Close()

	got, err := FetchPlaneTag(context.Background(), plane.URL, nil)
	if err != nil || got != "v9.9.9" {
		t.Fatalf("FetchPlaneTag = %q, %v; want v9.9.9", got, err)
	}
	// A plane that is not there is an error here, and PlaneUnreachable upstream —
	// never a bogus mismatch.
	if _, err := FetchPlaneTag(context.Background(), "http://127.0.0.1:1", nil); err == nil {
		t.Error("FetchPlaneTag succeeded against a closed port")
	}
}

func TestNewRejectsBadPlaneURL(t *testing.T) {
	for _, url := range []string{"", "   ", "not a url", "ftp://example.com", "http://"} {
		if _, err := New(Config{PlaneURL: url, FrontendFS: nil}); err == nil {
			t.Errorf("New(%q) succeeded, want an error", url)
		}
	}
}
