package db

import (
	"strings"
	"testing"
)

// TestStablePromptPrefixMemoryPlaybook pins the memory playbook block: the
// four durable memory tools must be named in StablePromptPrefix (the cached
// shared prefix every worker receives), and the prefix must stay
// byte-identical for identical inputs (KV/prompt-cache reuse).
func TestStablePromptPrefixMemoryPlaybook(t *testing.T) {
	p := StablePromptPrefix("test-image", "")
	for _, tool := range []string{"memory_search", "memory_write", "memory_read", "memory_list"} {
		if !strings.Contains(p, tool) {
			t.Errorf("StablePromptPrefix missing durable memory tool %q", tool)
		}
	}
	if !strings.Contains(p, "orchicon_memory_note") {
		t.Errorf("StablePromptPrefix missing the in-session orchicon_memory_note reference")
	}
	// Byte-identity: identical inputs produce byte-identical prefix bytes.
	if q := StablePromptPrefix("test-image", ""); q != p {
		t.Error("StablePromptPrefix must be byte-identical for identical inputs (cache-safe)")
	}
	// The runtime image is part of the prefix (per-run constant).
	if StablePromptPrefix("other-image", "") == p {
		t.Error("StablePromptPrefix must vary with the runtime image")
	}
}
