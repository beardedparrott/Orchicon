package db

import (
	"strings"
	"testing"
)

// WHY THIS FILE EXISTS.
//
// A QA worker on run 01M8CSC92YWEZSC2FTRWC2K5A7 reported three acceptance criteria NOT VERIFIED
// because they "need live paid agy calls and a plane restart that this QA step cannot run".
//
// It was not refusing, and it was not incapable. Our own prompts told it to refuse:
//
//	localEnvironmentBlock:  "127.0.0.1:5432, localhost:5432 and 172.17.0.1:8080 are the LIVE
//	                         Orchicon plane. NEVER point ORCHICON_TEST_DSN at them"
//	sandboxPlaneBlock:      "NEVER work in the prod instance of Orchicon: you ONLY work inside
//	                         the runtime container and launch your own disposable in-container
//	                         sandbox instance"
//	universalBlock:         "NEVER spend tokens testing a model ... make it ONE minimal bounded
//	                         call"
//
// Work item 6 was `execution_mode: local` — no container — so the "only work inside the runtime
// container" sentence described something it did not have, and the one plane it COULD reach was
// labelled LIVE and forbidden. The only self-consistent reading was "don't touch the plane". Then
// three ACs were graded as failures for not being done.
//
// That is a prompt defect being reported as a worker defect, and these tests exist so the policy
// cannot silently drift back into contradiction. The real ports: dev is 8080/5433, prod is 8091/5432.

const (
	devPort = "8080"
	devDB   = "5433"
	prodPort = "8091"
	prodDB   = "5432"
)

// TestLocalEnvironmentNamesDevAsTheTestTarget is the block a local-mode worker actually reads to
// decide whether it may touch a plane.
func TestLocalEnvironmentNamesDevAsTheTestTarget(t *testing.T) {
	blk := localEnvironmentBlock()

	if !strings.Contains(blk, prodPort) || !strings.Contains(blk, prodDB) {
		t.Errorf("the local environment block does not name PROD (%s, DB %s) — a worker cannot avoid "+
			"what it cannot identify", prodPort, prodDB)
	}
	if !strings.Contains(blk, devPort) {
		t.Errorf("the local environment block does not name DEV (%s) as a test target, so a local-mode "+
			"worker has no sanctioned plane and will (correctly) refuse to touch the one it has", devPort)
	}
	// The specific mislabel that caused the refusal must be gone.
	if strings.Contains(blk, "8080`/`:5432` on the host is the LIVE") ||
		strings.Contains(blk, "localhost:8080`/`:5432` on the host") {
		t.Error("the block still calls 8080/5432 jointly 'the LIVE plane' — 8080 is DEV and writable, " +
			"5432 is PROD and is not. Conflating them is what told QA to stand down")
	}
}

// TestProdIsStatedAsUntouchable: dev may be used, prod never. Both halves must be explicit, because
// a prompt that only says "never prod" leaves a worker to guess what it MAY do.
func TestProdIsStatedAsUntouchable(t *testing.T) {
	blk := localEnvironmentBlock()
	for _, want := range []string{"8091", "5432", "NEVER"} {
		if !strings.Contains(blk, want) {
			t.Errorf("the prod prohibition is missing %q", want)
		}
	}
}

// TestDevIsWritableAndRestartable is the permission that was missing. A worker may restart dev when
// a criterion needs it — otherwise "prove resume survives a restart" is an unverifiable criterion by
// construction.
func TestDevIsWritableAndRestartable(t *testing.T) {
	blk := localEnvironmentBlock()
	lower := strings.ToLower(blk)
	if !strings.Contains(lower, "restart") {
		t.Error("nothing tells a worker it may restart DEV, so any criterion that needs a restart is " +
			"unverifiable no matter how capable the worker is")
	}
	if !strings.Contains(lower, "delete") && !strings.Contains(lower, "clean") {
		t.Error("nothing tells a worker to clean up what it creates in dev")
	}
}

// TestSandboxPlaneBlockDoesNotForbidTheHostPlane: the universal block must not re-impose the
// prohibition that the environment block just lifted, or the two contradict each other again and the
// worker follows the stricter one.
func TestSandboxPlaneBlockDoesNotForbidTheHostPlane(t *testing.T) {
	lower := strings.ToLower(sandboxPlaneBlock)

	if strings.Contains(lower, "only work inside the runtime container") {
		t.Error("sandboxPlaneBlock still says a worker may ONLY work inside the runtime container — " +
			"in local mode there is no container, so this leaves no legal target at all")
	}
	if !strings.Contains(sandboxPlaneBlock, prodPort) {
		t.Error("sandboxPlaneBlock does not distinguish prod by its port, so 'never prod' is unactionable")
	}
	if !strings.Contains(sandboxPlaneBlock, devPort) {
		t.Error("sandboxPlaneBlock does not name dev as reachable")
	}
}

// TestLiveCallsRequiredByACriteriaArePermitted is the second half of the same defect: the token
// clause discouraged exactly the calls AC4-AC6 demanded.
func TestLiveCallsRequiredByACriteriaArePermitted(t *testing.T) {
	universal := safetyBlock
	if !strings.Contains(universal, "DO NOT SPEND TOKENS PROVING A MODEL IS GOOD") {
		t.Fatal("the safety block no longer carries the token-spend rule")
	}
	// It must distinguish "don't show off" from "do the evidence your criteria name".
	if !strings.Contains(universal, "WHEN AN ACCEPTANCE CRITERION NAMES A LIVE RUN") {
		t.Error("the token-spend rule no longer carves out live runs that an ACCEPTANCE CRITERION " +
			"names — a worker will again mark those NOT VERIFIED and be graded as failing")
	}
	if !strings.Contains(universal, "cheapest capable model") {
		t.Error("nothing bounds the cost of those calls, which is the legitimate half of the old rule")
	}
}

// TestAllAdaptersPermittedForTesting: the policy allows every adapter as a test subject, so a worker
// that needs agy to verify an agy criterion is not told to stick to fixtures.
func TestAllAdaptersPermittedForTesting(t *testing.T) {
	universal := safetyBlock
	for _, adapter := range []string{"orchicon", "opencode", "claude", "agy"} {
		if !strings.Contains(universal, adapter) {
			t.Errorf("adapter %q is not named as a permitted test subject", adapter)
		}
	}
}
