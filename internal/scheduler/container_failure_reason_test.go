package scheduler

// container_failure_reason_test.go — THE CONTAINER-FAILURE MESSAGE MUST NAME THE REAL CAUSE.
//
// Reported by an operator running a Quick Work flow on the opencode adapter: it "failed immediately
// complaining about the runtime image." The recorded reason was:
//
//	runtime container could not be created (image "X", then base "orchicon-runtime:base"):
//	  arm-site container ensure failed
//
// which names neither the Docker error nor which attempt produced it — because the shape it came from
// bound both EnsureForRun errors inside `if` statements and reported a PLACEHOLDER instead. The
// sentence says a container could not be created and refuses to say why, so the failure is
// un-diagnosable from outside the process.

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestContainerFailureReasonCarriesBothErrors(t *testing.T) {
	retryErr := errors.New("docker: Error response from daemon: pull access denied for x, repository does not exist")
	baseErr := errors.New("docker: no such image: orchicon-runtime:base")

	got := containerFailureReason("orchicon-runtime:orchicon-dev", retryErr, baseErr)

	// The FIRST attempt's error is what usually explains the failure, so it must be present verbatim.
	if !strings.Contains(got, retryErr.Error()) {
		t.Errorf("the resolved-image attempt's error is missing.\n  got: %s\n  want substring: %s", got, retryErr)
	}
	// And the fallback's error too — an operator needs to know WHICH image was refused.
	if !strings.Contains(got, baseErr.Error()) {
		t.Errorf("the base-image attempt's error is missing.\n  got: %s", got)
	}
	// Both images must be named, or "which image?" is unanswerable.
	for _, img := range []string{"orchicon-runtime:orchicon-dev", "orchicon-runtime:base"} {
		if !strings.Contains(got, img) {
			t.Errorf("image %q is not named in: %s", img, got)
		}
	}
}

func TestContainerFailureReasonDistinguishesAnUnattemptedBase(t *testing.T) {
	// When the resolved image WAS the base image, no fallback attempt is made. A bare %v would print
	// "<nil>" for that, which reads like a failure and hides that the base attempt never ran.
	retryErr := fmt.Errorf("no such image")
	got := containerFailureReason("orchicon-runtime:base", retryErr, nil)
	if !strings.Contains(got, "not attempted") {
		t.Errorf("an unattempted base image must be labelled, not rendered as <nil>.\n  got: %s", got)
	}
	if strings.Contains(got, "<nil>") {
		t.Errorf("the message must not contain a bare nil: %s", got)
	}
}
