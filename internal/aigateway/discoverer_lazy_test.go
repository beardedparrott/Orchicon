package aigateway

// discoverer_lazy_test.go — discovery must not require a plane restart.
//
// REPORTED: "model discovery is not configured for this plane - set ORCHICON_OPENCODE_BIN or install
// opencode on PATH - then restart the plane" (TUI) and the equivalent GUI message, on a machine where
// `opencode` ran fine from any shell. "... It should be auto discovering the location of opencode ...
// This should happen automatically when someone selects the opencode adapter in the TUI or the GUI."
//
// The old design could not do it: the discoverer was built once at boot, so a failed first probe was
// permanent and the only remedy was a restart. These assert the replacement rule:
//
//	no discoverer  ->  resolve on the request  ->  answer with models
//
// and that a FAILED resolution is not cached, because the whole point is that the next one retries.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// countingFactory hands out a discoverer on the Nth call, and counts how many times it was asked.
//
// IT COUNTS RATHER THAN ASSERTS THE DISCOVERER directly because the property under test is about
// WHEN resolution runs, not what it returns: "resolved on the request" and "resolved at
// construction" are behaviourally identical from the outside on the happy path, and differ entirely
// when the first attempt fails.
type countingFactory struct {
	mu     sync.Mutex
	calls  int
	succeedOn int // the call number (1-based) that starts succeeding; 0 means "always"
	err    error
	model  *ModelDiscoverer
}

func (f *countingFactory) build() (*ModelDiscoverer, *MCPDiscoverer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.succeedOn != 0 && f.calls < f.succeedOn {
		if f.err == nil {
			return nil, nil, errors.New("opencode binary not found")
		}
		return nil, nil, f.err
	}
	return f.model, NewMCPDiscoverer(discardLogger(), "/fake/opencode"), nil
}

func (f *countingFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestDiscoveryResolvesOnTheRequestNotJustAtBoot is the reported bug, end to end.
func TestDiscoveryResolvesOnTheRequestNotJustAtBoot(t *testing.T) {
	// A service that BOOT FAILED TO CONFIGURE: no discoverer, exactly as the plane builds it when the
	// probe at startup found nothing.
	svc := NewService(nil, discardLogger(), nil, nil, nil, nil, nil, nil)

	// ONE REQUEST WITH NO WAY TO RESOLVE: the message the operator saw, unchanged. A service with no
	// factory cannot do better than this and must not pretend otherwise.
	_, err := svc.ListOpenCodeModels(context.Background(), connect.NewRequest(&apiv1.ListOpenCodeModelsRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("with no factory, ListOpenCodeModels code = %v, want Unimplemented", got)
	}

	// NOW THE OPERATOR INSTALLS OPENCODE AND SELECTS THE ADAPTER. Boot is long past; nothing but this
	// request may do the work.
	f := &countingFactory{model: NewModelDiscoverer(discardLogger(), "/fake/opencode")}
	svc.SetDiscovererFactory(f.build)

	// The first selection resolves. It will then try to RUN the fake binary and fail — but the failure
	// must be an EXECUTION error, not Unimplemented: the difference is "the plane found opencode and
	// could not run my stub" versus "the plane never looked".
	_, err = svc.ListOpenCodeModels(context.Background(), connect.NewRequest(&apiv1.ListOpenCodeModelsRequest{}))
	if got := connect.CodeOf(err); got == connect.CodeUnimplemented {
		t.Fatalf("after installing opencode, ListOpenCodeModels is STILL Unimplemented — discovery "+
			"did not re-resolve on the request, so the operator must restart the plane and the "+
			"reported bug is unchanged (err=%v)", err)
	}
	if n := f.count(); n != 1 {
		t.Fatalf("factory called %d time(s), want exactly 1 for the first resolving request", n)
	}

	// AND IT IS NOT PROBED AGAIN ON SUBSEQUENT REQUESTS. Success is cached in the service; the
	// discoverer has its own model TTL behind that. Re-probing per request would be waste, and on
	// Windows it would hit the registry every time.
	_, _ = svc.ListOpenCodeModels(context.Background(), connect.NewRequest(&apiv1.ListOpenCodeModelsRequest{}))
	_, _ = svc.ListOpenCodeModels(context.Background(), connect.NewRequest(&apiv1.ListOpenCodeModelsRequest{}))
	if n := f.count(); n != 1 {
		t.Fatalf("factory called %d times after resolution succeeded, want 1 — a resolved discoverer "+
			"must be reused rather than re-probed on every model-tier request", n)
	}
}

// TestAFailedLateResolutionIsRetried — the other half, and the one that makes installing opencode
// work at all. Caching a FAILURE would be as broken as caching the boot result was.
func TestAFailedLateResolutionIsRetried(t *testing.T) {
	svc := NewService(nil, discardLogger(), nil, nil, nil, nil, nil, nil)
	f := &countingFactory{succeedOn: 3, model: NewModelDiscoverer(discardLogger(), "/fake/opencode")}
	svc.SetDiscovererFactory(f.build)

	// Two requests while opencode is still absent: each must try again, and each must answer
	// Unimplemented so the client shows the actionable message.
	for i := 1; i <= 2; i++ {
		_, err := svc.ListOpenCodeModels(context.Background(), connect.NewRequest(&apiv1.ListOpenCodeModelsRequest{}))
		if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
			t.Fatalf("request %d with no binary: code = %v, want Unimplemented", i, got)
		}
		if n := f.count(); n != i {
			t.Fatalf("after %d failed request(s) the factory was called %d time(s) — a failed "+
				"resolution must NOT be cached, or installing opencode later never takes effect", i, n)
		}
	}

	// Third attempt succeeds: opencode was installed between the second and third request.
	_, err := svc.ListOpenCodeModels(context.Background(), connect.NewRequest(&apiv1.ListOpenCodeModelsRequest{}))
	if got := connect.CodeOf(err); got == connect.CodeUnimplemented {
		t.Fatalf("discovery still Unimplemented on the attempt that resolved — a later success must "+
			"take effect without a restart (err=%v)", err)
	}
}

// TestListMCPsAlsoResolvesLazily — the same field feeds MCP discovery, and an operator who selects
// the opencode adapter for models will look for its MCP servers next.
func TestListMCPsAlsoResolvesLazily(t *testing.T) {
	svc := NewService(nil, discardLogger(), nil, nil, nil, nil, nil, nil)
	f := &countingFactory{model: NewModelDiscoverer(discardLogger(), "/fake/opencode")}
	svc.SetDiscovererFactory(f.build)

	_, err := svc.ListOpenCodeMCPs(context.Background(), connect.NewRequest(&apiv1.ListOpenCodeMCPsRequest{}))
	if got := connect.CodeOf(err); got == connect.CodeUnimplemented {
		t.Fatalf("ListOpenCodeMCPs is still Unimplemented after a factory was installed — MCP "+
			"discovery stayed boot-only, so the two halves disagree (err=%v)", err)
	}
	if n := f.count(); n != 1 {
		t.Fatalf("factory called %d time(s) for one MCP request, want 1", n)
	}
}
