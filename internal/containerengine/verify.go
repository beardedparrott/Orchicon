package containerengine

import (
	"context"
	"fmt"
	"time"
)

// DefaultVerifyImage is the trivial image Verify runs. It is deliberately tiny and
// universally available: the point is to prove the engine can PULL AND RUN, not to test
// anything about our own images.
const DefaultVerifyImage = "docker.io/library/alpine:3"

// DefaultVerifyTimeout bounds a verify run. A first run may pull the image, so this is
// generous; it is not a health check, it is a one-shot proof.
const DefaultVerifyTimeout = 5 * time.Minute

// Verify proves the engine WORKS by running a trivial container (AC4) — "installed" is
// not "working". A PATH probe and even `podman info` can succeed on a host whose engine
// cannot pull or run anything (a stopped VM, a broken storage driver, a rootless mapping
// that resolves to nothing); the only honest proof is a container that exits 0.
//
// The image is a parameter so a test — and an air-gapped operator — can name a local one.
// A timeout of 0 takes DefaultVerifyTimeout. The engine's own output is carried in the
// error, because "exit status 125" alone tells nobody anything.
func Verify(ctx context.Context, p Probe, e Engine, image string, timeout time.Duration) error {
	if e.Name == EngineNone || e.Bin == "" {
		return fmt.Errorf("no container engine to verify")
	}
	if image == "" {
		image = DefaultVerifyImage
	}
	if timeout <= 0 {
		timeout = DefaultVerifyTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := p.Run(runCtx, e.Bin, "run", "--rm", image, "/bin/true")
		done <- result{out, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			detail := firstLine(res.out)
			if detail == "" {
				detail = res.err.Error()
			}
			return fmt.Errorf("the container engine is installed but cannot run a container: %s", detail)
		}
		return nil
	case <-runCtx.Done():
		return fmt.Errorf("the container engine did not finish a trivial container run within %s (%v)", timeout, runCtx.Err())
	}
}
