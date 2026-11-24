package adapter

// adapter_totality_test.go — THE ADD-A-KIND SWEEP, ASSERTED.
//
// Adding an adapter kind is not a one-line registration. It has now cost this platform three
// outages (opencode, orchicon, claude) because the kind was added to ONE enumeration and forgotten
// in another, and the symptom is always the same opaque runtime failure. The three tests in this
// file and its siblings (internal/runtime/adapter_totality_test.go and
// internal/server/adapter_totality_test.go) each iterate adapter.BuiltinAdapterKinds() — the LIVE
// catalog — so a kind added there and not here FAILS until every enumeration catches up:
//
//	THIS FILE        binary discovery (adapterpath): env override + a resolvable install
//	runtime/…        boot profile classification (classifyAdapter) and the host-fingerprint loop
//	server/…         dispatcher registration and the READY runtime_adapters seed row
//
// The catalog is the seed of the sweep because it is the one list a new kind must be added to for
// anything else to work at all, which makes it the honest anchor for totality.

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestAdapterKindTotalityBinaryDiscovery asserts every catalog kind is DISCOVERABLE: it names an
// environment override and its CLI is found at the declared install location on a POSIX host.
func TestAdapterKindTotalityBinaryDiscovery(t *testing.T) {
	kinds := catalogKindsSorted(t)

	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			// THE NATIVE ADAPTER IS THE PRODUCT BINARY, not an adapter CLI: it is bind-mounted at
			// /usr/local/bin/orchicon by the daemon and DiscoverKind deliberately refuses to find it.
			if kind == NativeAdapterKind {
				if got := BinEnv(kind); got != "" {
					t.Errorf("BinEnv(%q) = %q, want \"\" — the native adapter is the product binary and has no host install to override", kind, got)
				}
				if got := DiscoverKind("linux", "/home/u", kind, nil); got != nil {
					t.Errorf("DiscoverKind(%q) = %v, want nil — the product binary must never be discovered as an adapter CLI", kind, got)
				}
				return
			}

			if got := BinEnv(kind); got == "" {
				t.Errorf("adapter kind %q names no environment override — add a BinEnv case. Every other kind has "+
					"one (ORCHICON_OPENCODE_BIN / ORCHICON_CLAUDE_BIN / ORCHICON_AGY_BIN) and it is the ONLY way an "+
					"operator whose install is somewhere unusual can point the plane at it.", kind)
			}

			// A temp home carrying the binary EXACTLY where the declaration says it lives. If the
			// declared dir and the binary name disagree (the `antigravity` kind, whose CLI is `agy`),
			// this fails — which is the point: a fixed name derived from the kind is the bug.
			home := t.TempDir()
			dir := declaredBinDir(home, kind)
			if dir == "" {
				t.Fatalf("declaredBinDir(%q) is empty — add a case to adapterpath.go's declaredBinDir switch", kind)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(dir, binaryName("linux", kind))
			if err := os.WriteFile(want, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			// A PATH that cannot find the CLI, so only the DECLARED location can resolve it.
			emptyPath := t.TempDir()
			getenv := func(k string) string {
				if k == "PATH" {
					return emptyPath
				}
				return ""
			}
			if got := ResolveBinary("linux", home, kind, getenv); got != want {
				t.Errorf("ResolveBinary(linux, <home>, %q) = %q, want the declared install %q — the operator's CLI "+
					"is installed where the declaration says, so a miss here is 'installed but not found'", kind, got, want)
			}
		})
	}
}

// catalogKindsSorted returns the LIVE builtin catalog as a sorted slice, with an anti-vacuity guard.
func catalogKindsSorted(t *testing.T) []string {
	t.Helper()
	set := BuiltinAdapterKinds()
	if len(set) < 4 {
		t.Fatalf("the builtin catalog declared only %d adapter kind(s) (%v) — the LIVE kind source "+
			"(NewBuiltinProviderCatalog) shrank, so every totality assertion in this suite under-covers", len(set), set)
	}
	kinds := make([]string, 0, len(set))
	for k := range set {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}
