package scheduler

// baseimage.go — ONE barrier that makes the resolve chain's fallback an image the
// runtime daemon will actually accept.
//
// THE BUG THIS EXISTS FOR. The resolve chain (explicit work-item image -> project
// default -> base) ended at db.BaseRuntimeImage, the literal "orchicon-runtime:base",
// which is not a tag any host has. A release tags the base
// ghcr.io/beardedparrott/orchicon-runtime:latest and a dev host builds it as
// orchicon-runtime:local (runtimeimage/seed.go + seed_test.go's table), so the last
// leg of the chain named an image the daemon's allowlist refuses:
//
//	base "orchicon-runtime:base" -> image not allowed: orchicon-runtime:base
//
// A work item with no image of its own could therefore never get a container, and
// the failure looked like a container problem rather than a resolve-chain one.
//
// WHY A BARRIER AND NOT JUST A BETTER CONSTANT. The correct value is a property of
// the HOST, not of the source tree: it is whatever base tag the daemon on this
// machine has (ORCHICON_RUNTIME_IMAGE, default the published :latest). Only the
// daemon process knows it. So internal/db holds a placeholder that the daemon
// publishes into, and this function is the plane's half of that agreement — it runs
// the same resolution from the SAME environment variable, which is what makes a
// split deployment (daemon and plane as separate processes) agree with the
// in-process case.
//
// It is idempotent and cheap, so it is safe to call from every entry point that
// resolves an image.

import (
	"os"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// EnvRuntimeBaseImage is the environment variable the runtime daemon reads for its
// base image (`orchicon runtime-daemon`, cmd/orchicon/runtime.go). This package
// reads the SAME variable so the plane and the daemon cannot disagree about which
// tag the fallback names.
const EnvRuntimeBaseImage = "ORCHICON_RUNTIME_IMAGE"

// DefaultRuntimeBaseImage is the daemon's own default base tag. It must stay equal
// to the default in cmd/orchicon/runtime.go; the test beside this file pins the
// agreement so a change on one side cannot silently strand the other.
const DefaultRuntimeBaseImage = "ghcr.io/beardedparrott/orchicon-runtime:latest"

// UseDaemonBaseImage points the resolve chain's fallback at the base image the
// runtime daemon on this host uses.
//
// CALL IT BEFORE RESOLVING ANY IMAGE. Every leg of the chain funnels through
// imageForRun, which reads db.RuntimeBaseImage(), so this single assignment is
// enough for the whole chain.
func UseDaemonBaseImage() string {
	ref := strings.TrimSpace(os.Getenv(EnvRuntimeBaseImage))
	if ref == "" {
		ref = DefaultRuntimeBaseImage
	}
	db.SetBaseRuntimeImage(ref)
	return ref
}
