package runtime

import (
	"errors"
	"strings"
	"testing"
	"text/template"
)

// TestDetectEnginePrecedence pins the engine resolution order: explicit
// override, then podman, then docker, then a bare docker fallback. The image
// path is repointed by this ONE decision, so its precedence is a contract.
func TestDetectEnginePrecedence(t *testing.T) {
	both := func(name string) (string, error) {
		if name == EnginePodman || name == EngineDocker {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	dockerOnly := func(name string) (string, error) {
		if name == EngineDocker {
			return "/usr/bin/docker", nil
		}
		return "", errors.New("not found")
	}
	none := func(string) (string, error) { return "", errors.New("not found") }

	cases := []struct {
		name     string
		override string
		lookPath func(string) (string, error)
		wantBin  string
		wantName string
	}{
		{"podman wins when both are installed", "", both, EnginePodman, EnginePodman},
		{"docker is still supported", "", dockerOnly, EngineDocker, EngineDocker},
		{"no engine falls back to docker rather than failing", "", none, EngineDocker, EngineDocker},
		{"an explicit override wins over PATH", "/opt/bin/podman-remote", both, "/opt/bin/podman-remote", EnginePodman},
		{"an override with an unknown basename reports it", "nerdctl", both, "nerdctl", "nerdctl"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DetectEngine(c.override, c.lookPath)
			if got.Bin != c.wantBin || got.Name != c.wantName {
				t.Fatalf("DetectEngine(%q) = {%q,%q}, want {%q,%q}", c.override, got.Bin, got.Name, c.wantBin, c.wantName)
			}
		})
	}
}

// TestIsAbsentOutputBothEngines is the wording table for "that image is not
// here": Docker says "No such image", Podman says "image not known", and the
// removal path must treat both as the idempotent already-gone result. Anything
// else stays a real error.
func TestIsAbsentOutputBothEngines(t *testing.T) {
	e := Engine{Bin: EnginePodman, Name: EnginePodman}
	absent := []string{
		"Error: No such image: orchicon-runtime-custom:latest",
		"Error: orchicon-runtime:base: image not known",
		"Error: no such object: abc123",
	}
	for _, out := range absent {
		if !e.IsAbsentOutput(out) {
			t.Errorf("IsAbsentOutput(%q) = false, want true", out)
		}
	}
	present := []string{"", "Untagged: foo:latest", "Error: image is in use by a container", "permission denied"}
	for _, out := range present {
		if e.IsAbsentOutput(out) {
			t.Errorf("IsAbsentOutput(%q) = true, want false", out)
		}
	}
}

// TestLabelTemplateIsEnginePortable guards the inspect gate's format string:
// it must use only fields both engines expose (.Config.Labels) and must render
// correctly against a map[string]string of labels. The container-create gate
// depends on this string, so a Docker-only field creeping in here would
// silently open the gate under Podman.
func TestLabelTemplateIsEnginePortable(t *testing.T) {
	format := labelTemplate("org.orchicon.runtime-base")
	for _, dockerOnly := range []string{".HostConfig", ".GraphDriver", ".DockerVersion", ".RepoTags"} {
		if strings.Contains(format, dockerOnly) {
			t.Fatalf("label template %q uses the Docker-only field %s", format, dockerOnly)
		}
	}
	tmpl, err := template.New("label").Parse(format)
	if err != nil {
		t.Fatalf("label template does not parse: %v", err)
	}
	var sb strings.Builder
	err = tmpl.Execute(&sb, map[string]any{
		"Config": map[string]any{"Labels": map[string]string{"org.orchicon.runtime-base": "true"}},
	})
	if err != nil {
		t.Fatalf("label template does not render: %v", err)
	}
	if got := strings.TrimSpace(sb.String()); got != "true" {
		t.Fatalf("rendered label = %q, want true", got)
	}
}
