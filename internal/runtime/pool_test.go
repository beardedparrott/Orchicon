package runtime

import "testing"

// TestPoolEnvKeyStabilityAndInvalidation is the pool env-key contract: the
// key is a stable function of (image, sorted mounts, host fingerprint) — the
// same environment always produces the same key (warm reuse) and a change to
// ANY input produces a different key (fresh create). Mount order must not
// matter (the key hashes a sorted mount set).
func TestPoolEnvKeyStabilityAndInvalidation(t *testing.T) {
	req := CreateRequest{Image: "img:v1", Mounts: []MountSpec{{Source: "/a", Dest: "/b"}}}
	key := poolEnvKey(req, "hostfp")
	if poolEnvKey(req, "hostfp") != key {
		t.Fatalf("same env must produce the same key: %q != %q", poolEnvKey(req, "hostfp"), key)
	}

	// Mount order-independence.
	reqAB := CreateRequest{Image: "img:v1", Mounts: []MountSpec{{Source: "/a", Dest: "/b"}, {Source: "/c", Dest: "/d"}}}
	reqBA := CreateRequest{Image: "img:v1", Mounts: []MountSpec{{Source: "/c", Dest: "/d"}, {Source: "/a", Dest: "/b"}}}
	if k1, k2 := poolEnvKey(reqAB, "hostfp"), poolEnvKey(reqBA, "hostfp"); k1 != k2 {
		t.Fatalf("mount order must not change the key: %q != %q", k1, k2)
	}

	// Each input changed → different key.
	changed := map[string]string{
		"image":  poolEnvKey(CreateRequest{Image: "img:v2", Mounts: req.Mounts}, "hostfp"),
		"mount":  poolEnvKey(CreateRequest{Image: "img:v1", Mounts: []MountSpec{{Source: "/x", Dest: "/b"}}}, "hostfp"),
		"hostFp": poolEnvKey(req, "other-hostfp"),
	}
	if changed["image"] == key {
		t.Fatalf("image change must invalidate the key")
	}
	if changed["mount"] == key {
		t.Fatalf("mount change must invalidate the key")
	}
	if changed["hostFp"] == key {
		t.Fatalf("host fingerprint change must invalidate the key")
	}

	// The serve config (OPENCODE_CONFIG_CONTENT: worktree MCP base dir,
	// plane channel env, permission rules) is baked into the container at
	// create time and can never change on a live container. A run whose
	// serve config differs MUST NOT reuse a warm container baked with a
	// stale config — a container created before the plane channel existed
	// (or carrying a different run's worktree base / plane token) would
	// serve stale, wrong-scope tools and credentials. Empty configs fold
	// away (identical for non-serve requests).
	withCfg := CreateRequest{Image: "img:v1", Mounts: req.Mounts, ServeConfig: `{"mcp":{"orchicon-plane":{"enabled":true}}}`}
	keyCfg := poolEnvKey(withCfg, "hostfp")
	if keyCfg == key {
		t.Fatalf("serve config presence must invalidate the key (stale-config reuse)")
	}
	if poolEnvKey(withCfg, "hostfp") != keyCfg {
		t.Fatalf("same serve config must produce the same key")
	}
	otherCfg := CreateRequest{Image: "img:v1", Mounts: req.Mounts, ServeConfig: `{"mcp":{"orchicon-plane":{"enabled":true},"orchicon-worktree":{"enabled":true}}}`}
	if poolEnvKey(otherCfg, "hostfp") == keyCfg {
		t.Fatalf("different serve config must invalidate the key")
	}

	// Empty host fingerprint folds away: identical image+mounts still match.
	noFp := CreateRequest{Image: "img:v1", Mounts: req.Mounts}
	if poolEnvKey(noFp, "") != poolEnvKey(noFp, "") {
		t.Fatalf("identical env with empty host fp must produce the same key")
	}
}
