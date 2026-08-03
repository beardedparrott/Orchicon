package runtime

import (
	"strings"
	"testing"
)

// TestRewriteDockerfileBase verifies the base-inclusion guarantee: the
// daemon ALWAYS rewrites the FROM line to its own base image and injects
// the runtime-base label, regardless of what the author's Dockerfile
// says. User FROM lines are stripped so a derived image can never point
// at a different base.
func TestRewriteDockerfileBase(t *testing.T) {
	df := "FROM ubuntu:24.04\nUSER root\nRUN apt-get install -y xyz\nUSER orchicon\n"
	got := rewriteDockerfileBase(df, "orchicon-runtime:local")
	if !strings.HasPrefix(got, "FROM orchicon-runtime:local\n") {
		t.Errorf("expected daemon base first, got:\n%s", got)
	}
	if !strings.Contains(got, "LABEL org.orchicon.runtime-base=true") {
		t.Errorf("expected runtime-base label injected, got:\n%s", got)
	}
	if strings.Contains(got, "ubuntu") {
		t.Errorf("user FROM leaked into output:\n%s", got)
	}
	if !strings.Contains(got, "apt-get install -y xyz") {
		t.Errorf("user RUN lines lost:\n%s", got)
	}
}

// TestRewriteDockerfileBaseNoUserFrom handles a Dockerfile with no FROM
// at all (the service-generated easy-mode content already omits it).
func TestRewriteDockerfileBaseNoUserFrom(t *testing.T) {
	got := rewriteDockerfileBase("USER root\nRUN echo hi\n", "base:1")
	if !strings.HasPrefix(got, "FROM base:1\n") {
		t.Errorf("expected FROM base first, got:\n%s", got)
	}
	if !strings.Contains(got, "echo hi") {
		t.Errorf("user content lost")
	}
}

// TestImageTagPattern accepts valid image refs and rejects junk.
func TestImageTagPattern(t *testing.T) {
	for _, ok := range []string{"orchicon-runtime:latest", "ghcr.io/org/name:v1", "foo-bar:baz.1"} {
		if !imageTagPattern.MatchString(ok) {
			t.Errorf("expected %q to match", ok)
		}
	}
	for _, bad := range []string{"", "UPPER:tag", "no-tag", "a b:c", "name:"} {
		if imageTagPattern.MatchString(bad) {
			t.Errorf("expected %q to NOT match", bad)
		}
	}
}

// TestSlugPattern verifies the image slug rule matches project slugs.
func TestSlugPattern(t *testing.T) {
	for _, ok := range []string{"pyside6-gui", "base", "a-b-c"} {
		if !slugPattern.MatchString(ok) {
			t.Errorf("expected %q to match", ok)
		}
	}
	for _, bad := range []string{"", "UPPER", "-lead", "trail-", "a--b", "a b"} {
		if slugPattern.MatchString(bad) {
			t.Errorf("expected %q to NOT match", bad)
		}
	}
}

// TestAllowedImagesAlwaysIncludesBase verifies you can add images but
// never remove the base from the daemon's dropdown list.
func TestAllowedImagesAlwaysIncludesBase(t *testing.T) {
	d := &Daemon{Image: "base:1", Images: []string{"base:1", "gui:2"}}
	got := d.allowedImages()
	if got[0] != "base:1" {
		t.Errorf("base must be first, got %v", got)
	}
	found := 0
	for _, img := range got {
		if img == "base:1" {
			found++
		}
	}
	if found != 1 {
		t.Errorf("base must appear exactly once, got %d", found)
	}
}
