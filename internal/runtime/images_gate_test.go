package runtime

// images_gate_test.go — the CREATE GATE is a security control, not a
// convenience: it is what stops the plane from creating an arbitrary
// container image on the host. AC 2 of the Podman image-path change requires a
// test that asserts the REJECTION (not just that the allowlist helper returns
// a list), and that the decision is made against the engine under both
// engines' wording — Podman says "image not known", Docker says "No such
// image", and an image that is present but carries no
// org.orchicon.runtime-base marker must be refused just like a missing one.
//
// Every case drives the REAL gate (validateCreate -> imageAllowed) with the
// engine seam (Daemon.dockerFn) so no container engine is needed.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gateEngine builds a dockerFn that records the inspect it was asked to run
// and answers with the given label value / error, which is exactly the shape
// `image inspect --format '{{index .Config.Labels "..."}}' <ref>` produces.
func gateEngine(t *testing.T, wantRef, labelOut, stderr string, err error) (*[]string, func(args ...string) (string, error)) {
	t.Helper()
	var calls []string
	fn := func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if len(args) != 5 || args[0] != "image" || args[1] != "inspect" || args[2] != "--format" {
			t.Fatalf("the gate ran an unexpected engine command %q", strings.Join(args, " "))
		}
		if !strings.Contains(args[3], "org.orchicon.runtime-base") {
			t.Fatalf("the gate must read the runtime-base label, got format %q", args[3])
		}
		if args[4] != wantRef {
			t.Fatalf("the gate inspected %q, want %q", args[4], wantRef)
		}
		return labelOut, err
	}
	return &calls, fn
}

// TestCreateGateRejectsAnImageWithoutTheMarker is AC 2: an image that is
// neither the base, a configured stock variant, nor locally-present WITH the
// inherited runtime-base marker cannot be used to create a container.
func TestCreateGateRejectsAnImageWithoutTheMarker(t *testing.T) {
	const base = "orchicon-runtime:base"
	const stock = "ghcr.io/beardedparrott/orchicon-runtime:gui"
	const foreign = "docker.io/library/ubuntu:24.04"

	cases := []struct {
		name     string
		images   []string
		image    string
		labelOut string
		stderr   string
		err      error
		wantErr  string // "" means the gate must ALLOW
		// wantInspect: the decision must come from an engine label read.
		// The base and configured stock variants short-circuit on the
		// allowlist and must NOT touch the engine at all.
		wantInspect bool
	}{
		{
			name:        "an absent foreign image is rejected (Podman wording)",
			image:       foreign,
			stderr:      "Error: docker.io/library/ubuntu:24.04: image not known",
			err:         errors.New("exit status 125"),
			wantErr:     "image not allowed",
			wantInspect: true,
		},
		{
			name:        "an absent foreign image is rejected (Docker wording)",
			image:       foreign,
			stderr:      "Error: No such image: docker.io/library/ubuntu:24.04",
			err:         errors.New("exit status 1"),
			wantErr:     "image not allowed",
			wantInspect: true,
		},
		{
			name:        "a present image WITHOUT the marker is rejected",
			image:       foreign,
			labelOut:    "\n",
			wantErr:     "image not allowed",
			wantInspect: true,
		},
		{
			name:        "a present image whose marker is false is rejected",
			image:       foreign,
			labelOut:    "false\n",
			wantErr:     "image not allowed",
			wantInspect: true,
		},
		{
			name:        "a locally-present image carrying the marker is allowed",
			image:       "orchicon-runtime-custom:qa",
			labelOut:    "true\n",
			wantInspect: true,
		},
		{
			name:  "the daemon base image is always allowed",
			image: base,
		},
		{
			name:   "a configured stock variant is allowed",
			images: []string{base, stock},
			image:  stock,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Daemon{Image: base, Images: c.images, DockerBin: EngineDocker}
			// Cases that must be decided without asking the engine get a
			// seam that fails loudly if it is consulted at all.
			engineConsulted := false
			calls, fn := gateEngine(t, c.image, c.labelOut, c.stderr, c.err)
			d.dockerFn = func(args ...string) (string, error) {
				engineConsulted = true
				return fn(args...)
			}
			err := d.validateCreate(CreateRequest{WorkflowID: "wf-gate", Image: c.image})
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("gate must ALLOW %q, got error: %v (engine calls: %v)", c.image, err, *calls)
				}
				if engineConsulted != c.wantInspect {
					t.Fatalf("%q: engine consulted = %v, want %v (calls: %v)", c.image, engineConsulted, c.wantInspect, *calls)
				}
				return
			}
			if err == nil {
				t.Fatalf("gate must REJECT %q — a container could be created from an unvetted image", c.image)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("rejection must say %q, got %v", c.wantErr, err)
			}
			if engineConsulted != c.wantInspect {
				t.Fatalf("%q: engine consulted = %v, want %v — the rejection must be an engine-backed check", c.image, engineConsulted, c.wantInspect)
			}
		})
	}
}

// TestImageInspectJoinsPresenceAndLabels pins the single inspect that reports
// presence + both version labels for GET /v1/images (AC 7): a missing image
// must report Present=false with NO error, and a present one must report both
// labels — that is what lets the stock seeder tell "not downloaded" apart from
// "stale, roll it forward".
func TestImageInspectJoinsPresenceAndLabels(t *testing.T) {
	d := &Daemon{DockerBin: EnginePodman}
	d.dockerFn = func(args ...string) (string, error) {
		if len(args) != 5 || args[0] != "image" || args[1] != "inspect" {
			t.Fatalf("unexpected engine call %v", args)
		}
		if !strings.Contains(args[3], "org.orchicon.runtime.version") ||
			!strings.Contains(args[3], "org.orchicon.runtime.spec-version") {
			t.Fatalf("the report must ask for both version labels, got %q", args[3])
		}
		return "2026.02.1|12\n", nil
	}
	got := d.imageReport("orchicon-runtime:base")
	if !got.Present || got.RuntimeVersion != "2026.02.1" || got.SpecVersion != "12" {
		t.Fatalf("imageReport = %+v, want present with both labels", got)
	}

	// Absent: Podman reports an inspect failure — presence must be false and
	// the labels empty, with no error surfacing to the caller.
	d.dockerFn = func(args ...string) (string, error) {
		return "Error: orchicon-runtime:gui: image not known", errors.New("exit status 125")
	}
	absent := d.imageReport("orchicon-runtime:gui")
	if absent.Present || absent.RuntimeVersion != "" || absent.SpecVersion != "" {
		t.Fatalf("a missing image must report Present=false with no labels, got %+v", absent)
	}
}

// TestRemoveImageIsIdempotentAndRefusesJunk is the daemon half of AC 5: the
// removal path runs `rmi -f` for a plain image ref, reports "gone" when the
// engine says the image was never there (both engines' wording), and never
// reaches the engine for a ref that is not a plain tag.
func TestRemoveImageIsIdempotentAndRefusesJunk(t *testing.T) {
	const ref = "orchicon-runtime-custom:qa"

	var got []string
	d := &Daemon{DockerBin: EnginePodman}
	d.dockerFn = func(args ...string) (string, error) {
		got = args
		return "", nil
	}
	rec := httptest.NewRecorder()
	d.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/images?ref="+ref, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed"`) {
		t.Fatalf("want 200 removed, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Join(got, " ") != "rmi -f "+ref {
		t.Fatalf("removal must run `rmi -f <ref>`, got %v", got)
	}

	for _, wording := range []string{
		"Error: " + ref + ": image not known", // podman
		"Error: No such image: " + ref,        // docker
	} {
		d = &Daemon{DockerBin: EnginePodman}
		d.dockerFn = func(args ...string) (string, error) { return wording, errors.New("exit status 125") }
		rec = httptest.NewRecorder()
		d.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/images?ref="+ref, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"gone"`) {
			t.Fatalf("an already-absent image must be a no-op 200 gone (%q), got %d: %s", wording, rec.Code, rec.Body.String())
		}
	}

	d = &Daemon{DockerBin: EnginePodman}
	d.dockerFn = func(args ...string) (string, error) {
		t.Fatalf("a rejected ref must not reach the engine, got %v", args)
		return "", errors.New("unreachable")
	}
	rec = httptest.NewRecorder()
	d.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/images?ref=$(whoami)", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a malformed ref, got %d", rec.Code)
	}
}

// TestBuildStreamsAndPrunesDangling is AC 3's positive half on the daemon
// side: a successful build relays the engine's stdout AND stderr as
// {stream,data} events, ends with an exit event, and prunes dangling images
// afterwards so repeated builds of one tag do not pile up layers.
func TestBuildStreamsAndPrunesDangling(t *testing.T) {
	dir := t.TempDir()
	engine := filepath.Join(dir, "engine")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\necho 'STEP 1/2: FROM base'\necho 'warning: something' >&2\n"), 0o755); err != nil {
		t.Fatalf("write stub engine: %v", err)
	}
	d := &Daemon{DockerBin: engine, Image: "orchicon-runtime:base"}
	var pruned []string
	d.dockerFn = func(args ...string) (string, error) {
		pruned = args
		return "", nil
	}
	body := `{"slug":"qa","tag":"orchicon-runtime-custom:qa","base_image_ref":"orchicon-runtime:base","dockerfile":"RUN echo hi\n"}`
	rec := httptest.NewRecorder()
	d.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("build must stream 200, got %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{`"stream":"stdout"`, "STEP 1/2", `"stream":"stderr"`, "warning: something", `"event":"exit"`} {
		if !strings.Contains(out, want) {
			t.Errorf("build log must contain %q, body=%s", want, out)
		}
	}
	if strings.Join(pruned, " ") != "image prune -f --filter dangling=true" {
		t.Fatalf("a successful build must prune dangling images, got %v", pruned)
	}
}
