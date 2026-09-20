package runtime

import "testing"

// TestPoolEnvKeyStabilityAndInvalidation is the pool env-key contract: the
// key is a stable function of (image, sorted mounts, host fingerprint) — the
// same environment always produces the same key (warm reuse) and a change to
// ANY input produces a different key (fresh create). Mount order must not
// matter (the key hashes a sorted mount set).
// TestPoolEnvKeyGitStrategyIsolation is the L1 pool gate: a `none`
// (ephemeral) container is created WITHOUT a push-capable GH_TOKEN and
// WITHOUT the git credential mounts, while a `local`/`pr` container carries
// them. The two must never pool — a `none` run reusing a warm token-bearing
// container is exactly the pre-fix credential-leak class (a stale container
// would serve a push-capable token to an ephemeral run). Same strategy must
// still hit the warm pool.
func TestPoolEnvKeyGitStrategyIsolation(t *testing.T) {
	base := CreateRequest{Image: "img:v1", Mounts: []MountSpec{{Source: "/a", Dest: "/b"}}}

	baseLocal := base
	baseLocal.GitStrategy = "local"
	baseNone := base
	baseNone.GitStrategy = "none"
	basePR := base
	basePR.GitStrategy = "pr"

	keyLocal := poolEnvKey(baseLocal, "hostfp")
	keyNone := poolEnvKey(baseNone, "hostfp")

	if keyLocal == keyNone {
		t.Fatalf("local and none strategies must not pool (same key %q)", keyLocal)
	}
	if poolEnvKey(baseLocal, "hostfp") != keyLocal {
		t.Fatalf("same local strategy must produce the same key")
	}
	if poolEnvKey(baseNone, "hostfp") != keyNone {
		t.Fatalf("same none strategy must produce the same key")
	}
	if poolEnvKey(basePR, "hostfp") == keyNone {
		t.Fatalf("pr and none strategies must not pool")
	}
	if keyLocal == "" || keyNone == "" {
		t.Fatalf("strategized requests must produce a non-empty key")
	}
}

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

// TestPoolEnvKeyBootProfileInvalidation is AC 4: the boot profile (the set of
// adapter kinds the run's step workers need) colors the pool key, so a
// container warmed for opencode is NEVER reused for a native-only run and vice
// versa. It also pins that the reset-shaped reconstruction (resetAndPool
// rebuilds the CreateRequest from the entry's own fields) re-keys identically
// — dropping the profile there would silently re-key the container under the
// wrong environment.
func TestPoolEnvKeyBootProfileInvalidation(t *testing.T) {
	base := CreateRequest{Image: "img:v1", Mounts: []MountSpec{{Source: "/a", Dest: "/b"}}}

	oc := base
	oc.AdapterKinds = []string{"opencode"}
	native := base
	native.AdapterKinds = []string{"orchicon"}
	mixed := base
	mixed.AdapterKinds = []string{"opencode", "orchicon"}
	mixedReordered := base
	mixedReordered.AdapterKinds = []string{"orchicon", "opencode"}
	explicitEmpty := base
	explicitEmpty.AdapterKinds = []string{}
	absent := base // nil: legacy "unspecified" profile

	kOC := poolEnvKey(oc, "hostfp")
	kNative := poolEnvKey(native, "hostfp")
	kMixed := poolEnvKey(mixed, "hostfp")
	kEmpty := poolEnvKey(explicitEmpty, "hostfp")
	kAbsent := poolEnvKey(absent, "hostfp")

	if kOC == kNative {
		t.Fatal("an opencode container and a native-only container must never pool (same key)")
	}
	if kOC == kMixed || kNative == kMixed {
		t.Fatal("a mixed profile must key distinctly from single-kind profiles")
	}
	if kEmpty == kOC || kEmpty == kNative || kEmpty == kAbsent {
		t.Fatal("an explicitly empty profile must key distinctly (it demands no adapter kind)")
	}
	if kAbsent == kNative {
		t.Fatal("the legacy absent profile resolves to the default demand and must not pool with native-only")
	}
	if poolEnvKey(oc, "hostfp") != kOC {
		t.Fatal("the same boot profile must produce the same key")
	}
	if poolEnvKey(mixedReordered, "hostfp") != kMixed {
		t.Fatal("profile order must not change the key (the key hashes a sorted profile)")
	}

	// resetAndPool rebuilds the request from the pool entry's own fields; the
	// boot profile MUST ride along, or the reset container re-keys wrong.
	rebuilt := CreateRequest{
		Image:        oc.Image,
		Mounts:       oc.Mounts,
		ServeConfig:  oc.ServeConfig,
		ProjectDir:   oc.ProjectDir,
		AdapterKinds: oc.AdapterKinds,
	}
	if got := poolEnvKey(rebuilt, "hostfp"); got != kOC {
		t.Fatalf("a reset-shaped rebuild must keep the key: %q != %q", got, kOC)
	}
}
