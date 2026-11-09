package runtime

// tmpfs_engine_grammar_test.go — THE OPTIONS WE HAND THE ENGINE MUST BE OPTIONS IT
// ACCEPTS, PER ENGINE FAMILY.
//
// THE DEFECT THIS PINS. Every runtime run failed before a worker started:
//
//	runtime daemon: create runtime orchicon-runtime-…:
//	  Error: unknown mount option "uid=1000": invalid mount option
//	  : exit status 125
//
// The daemon emitted Docker's ownership options (`uid=`, `gid=`) inside `--tmpfs`,
// and the engine it now drives is PODMAN, which rejects them. Nothing caught it
// because the existing tests asserted the argv CONTAINED those strings — which it
// did. They pinned the spelling, never the engine's acceptance of it. So the suite
// was green while no container could be created at all.
//
// Measured against the real binaries (podman 6.1.0, docker 29.7.2) to write this:
//
//	docker --tmpfs /p:rw,uid=1000,gid=1000,mode=0755   ACCEPT
//	podman --tmpfs /p:rw,uid=1000,gid=1000,mode=0755   REJECT  unknown mount option "uid=1000"
//	podman --tmpfs /p:rw,mode=0755                     ACCEPT
//	podman --tmpfs /p:rw,mode=1777                     ACCEPT
//	podman --mount type=tmpfs,…,uid=1000               REJECT  "uid: invalid mount option"
//
// The rule asserted here is therefore: PODMAN'S ARGV CARRIES NO uid=/gid= ANYWHERE,
// and the writability that those options were doing is delivered by MODE instead.
// Docker keeps its precise ownership, which it accepts.

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestPodmanTmpfsCarriesNoOwnershipOptions is the direct regression: no `--tmpfs` element in a
// podman argv may contain uid= or gid=, on ANY mount.
func TestPodmanTmpfsCarriesNoOwnershipOptions(t *testing.T) {
	d, _ := podmanCreateDaemon(t)
	d.TmpfsSize, d.HomeTmpfsSize = "2g", "2g"

	full := append(d.createFlagArgs("orchicon-rt-1", CreateRequest{WorkflowID: "wf-1", InstanceID: "dev"}),
		d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode"}, GitStrategy: "local"})...)

	sawTmpfs := 0
	for i := 0; i+1 < len(full); i++ {
		if full[i] != "--tmpfs" {
			continue
		}
		opt := full[i+1]
		sawTmpfs++
		for _, banned := range []string{"uid=", "gid="} {
			if strings.Contains(opt, banned) {
				t.Errorf("podman --tmpfs %q contains %q — podman rejects the whole create with "+
					"`unknown mount option %q: invalid mount option` (exit 125). Ownership must be "+
					"expressed with mode= under podman, not uid=/gid=.", opt, banned, banned)
			}
		}
	}
	// A vacuity guard: if the argv stopped carrying tmpfs entries at all, the loop above would pass
	// without checking anything — the same blind spot that let the original defect through.
	if sawTmpfs == 0 {
		t.Fatal("the podman create argv carries no --tmpfs at all, so this test proved nothing")
	}
}

// TestDockerTmpfsKeepsItsOwnershipOptions pins the OTHER half: docker is not being silently changed
// to the podman spelling.
func TestDockerTmpfsKeepsItsOwnershipOptions(t *testing.T) {
	d, home := podmanCreateDaemon(t)
	d.Engine = Engine{Name: "docker", Bin: "docker"}

	args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode"}, GitStrategy: "local"})
	// THE DAEMON'S REAL IDS, not a placeholder: docker receives the host uid/gid, and
	// that is exactly the ownership the container relies on for a writable $HOME.
	suffix := tmpfsOwnershipSuffixFor(false, d.UserID, d.GroupID)
	for _, base := range []string{home, filepath.Join(home, ".local"), filepath.Join(home, ".config")} {
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--tmpfs" && strings.HasPrefix(args[i+1], base+":rw"+suffix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("docker must keep its precise tmpfs ownership for %q (suffix %q): %v", base, suffix, args)
		}
	}
}

// TestTmpfsOwnershipIsChosenByEngineNotByPlatform is the macOS/Windows guarantee, stated as a test
// because it is the thing most likely to be "fixed" wrongly.
func TestTmpfsOwnershipIsChosenByEngineNotByPlatform(t *testing.T) {
	podman := tmpfsOwnershipSuffix(true)
	docker := tmpfsOwnershipSuffix(false)

	if podman == docker {
		t.Fatalf("the two engines are given the same ownership suffix %q — one of them is being "+
			"handed a grammar it refuses", podman)
	}
	if strings.Contains(podman, "uid=") || strings.Contains(podman, "gid=") {
		t.Errorf("the podman suffix %q uses uid=/gid=, which podman rejects on every platform "+
			"(Linux natively; macOS/Windows inside podman machine)", podman)
	}
	// mode= is the portable option: BOTH engines accept it, which is exactly why it
	// can carry podman's ownership without breaking docker.
	if !strings.Contains(podman, "mode=") {
		t.Errorf("the podman suffix %q carries no mode=, so the runtime uid would have no way "+
			"to write its mounts", podman)
	}
	if !strings.Contains(docker, "mode=") {
		t.Errorf("the docker suffix %q carries no mode=; docker needs the mode for directories the "+
			"uid alone does not make traversable", docker)
	}
}

// TestPodmanTmpfsModeIsWritableNotSticky pins 0777 rather than the tempting 1777.
func TestPodmanTmpfsModeIsWritableNotSticky(t *testing.T) {
	for _, tc := range []struct {
		podman bool
		want   string
	}{
		{true, ",mode=0777"},
		{false, ",uid=0,gid=0,mode=0755"},
	} {
		got := tmpfsOwnershipSuffix(tc.podman)
		if got != tc.want {
			t.Errorf("tmpfsOwnershipSuffix(podman=%v) = %q, want %q", tc.podman, got, tc.want)
		}
	}
}
