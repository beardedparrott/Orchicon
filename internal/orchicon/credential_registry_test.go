package orchicon

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --- Credential resolution (D7): tenant secret → env → actionable failure ------

func TestCredentialEnvResolution(t *testing.T) {
	r := NewCredentialResolver(nil, nil)
	r.Env = func(k string) string {
		if k == "ANTHROPIC_API_KEY" {
			return "env-key"
		}
		return ""
	}
	p, _ := BuiltinProfile("anthropic")
	v, err := r.Resolve(context.Background(), "tenant-1", p)
	if err != nil || v != "env-key" {
		t.Fatalf("env resolution = %q err=%v", v, err)
	}
}

func TestCredentialNoAuthProfile(t *testing.T) {
	r := NewCredentialResolver(nil, nil)
	p, _ := BuiltinProfile("ollama")
	v, err := r.Resolve(context.Background(), "tenant-1", p)
	if err != nil || v != "" {
		t.Fatalf("no-auth profile must resolve empty: %q %v", v, err)
	}
}

func TestCredentialActionableFailure(t *testing.T) {
	r := NewCredentialResolver(nil, nil)
	r.Env = func(string) string { return "" }
	p, _ := BuiltinProfile("anthropic")
	_, err := r.Resolve(context.Background(), "tenant-1", p)
	if !errors.Is(err, ErrAuthMissing) {
		t.Fatalf("ErrAuthMissing expected: %v", err)
	}
	// The failure NAMES what to set — actionable.
	for _, want := range []string{"ANTHROPIC_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must name %q: %v", want, err)
		}
	}
}

func TestCredentialRegistryRejectsUnresolved(t *testing.T) {
	r := NewCredentialResolver(nil, nil)
	r.Env = func(string) string { return "" }
	reg := NewRegistry(r, NewSourcingService(nil, nil), nil, nil)
	_, err := reg.Get(context.Background(), "tenant-1", "anthropic")
	if !errors.Is(err, ErrAuthMissing) {
		t.Fatalf("registry must surface ErrAuthMissing: %v", err)
	}
}

// --- Registry ------------------------------------------------------------------

func TestRegistryCachesInstances(t *testing.T) {
	r := NewCredentialResolver(nil, nil)
	r.Env = func(string) string { return "env-key" }
	reg := NewRegistry(r, NewSourcingService(nil, nil), nil, nil)
	p1, err := reg.Get(context.Background(), "t1", "openai")
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := reg.Get(context.Background(), "t1", "openai")
	if p1 != p2 {
		t.Fatal("same (tenant,provider) must return the cached instance")
	}
	p3, _ := reg.Get(context.Background(), "t2", "openai")
	if p1 == p3 {
		t.Fatal("different tenants must get distinct instances")
	}
	reg.Invalidate("t1", "openai")
	p4, _ := reg.Get(context.Background(), "t1", "openai")
	if p1 == p4 {
		t.Fatal("invalidate must drop the cached instance")
	}
}

func TestRegistryUnknownProvider(t *testing.T) {
	reg := NewRegistry(NewCredentialResolver(nil, nil), NewSourcingService(nil, nil), nil, nil)
	_, err := reg.Get(context.Background(), "t1", "nope")
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("unknown provider: %v", err)
	}
}

func TestRegistryCapabilities(t *testing.T) {
	r := NewCredentialResolver(nil, nil)
	r.Env = func(string) string { return "k" }
	reg := NewRegistry(r, NewSourcingService(nil, nil), nil, nil)
	anth, err := reg.Get(context.Background(), "t1", "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	cap := anth.Capabilities()
	if !cap.Streaming || !cap.Tools || !cap.CacheBreakpoints {
		t.Fatalf("anthropic caps = %+v", cap)
	}
	cc, err := reg.Get(context.Background(), "t1", "commandcode")
	if err != nil {
		t.Fatal(err)
	}
	ccCap := cc.Capabilities()
	if !ccCap.Streaming || !ccCap.Tools {
		t.Fatalf("commandcode caps = %+v", ccCap)
	}
}
