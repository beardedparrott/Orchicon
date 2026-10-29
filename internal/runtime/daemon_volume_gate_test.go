package runtime

// daemon_volume_gate_test.go — BF-3(a): NO NAMED VOLUMES (AC 9).
//
// The pristine reset's premise is "no writable state survives `rm -f`". Every
// mount therefore has to be a BIND from an absolute host path the daemon is
// allowed to expose. A named volume would be engine-managed storage that
// outlives the container, so the reset would silently stop being a reset — the
// run boundary would leak across runs.
//
// validateCreate formerly rejected a volume name only IMPLICITLY (a name is not
// an absolute path under an allowed root). Implicit is not a proof: it depends
// on the allowed-roots check staying first and on nobody ever relaxing it. This
// file pins the gate explicitly and pins the ARGV it must produce.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateCreateRejectsNamedVolumes is the gate: a bare volume name is
// refused with the pristine-reset reason in the message, not a generic
// "outside allowed roots".
func TestValidateCreateRejectsNamedVolumes(t *testing.T) {
	root := t.TempDir()
	d := &Daemon{AllowedRoots: []string{root}, Image: "orchicon-runtime:local"}

	for _, src := range []string{"orchicon-data", "./relative/path", "named-volume:/x"} {
		err := d.validateCreate(context.Background(), CreateRequest{
			WorkflowID: "wf-1",
			Mounts:     []MountSpec{{Source: src, Dest: "/work"}},
		})
		if err == nil {
			t.Fatalf("mount source %q was ACCEPTED — a non-absolute source is a name/relative path the "+
				"engine may resolve to storage that survives the pristine reset", src)
		}
		if !strings.Contains(err.Error(), "absolute host path") {
			t.Fatalf("mount source %q was rejected for the wrong reason: %v (the message must name the "+
				"pristine-reset rule so the next reader cannot relax it)", src, err)
		}
	}

	// A genuine bind under an allowed root still passes.
	if err := d.validateCreate(context.Background(), CreateRequest{
		WorkflowID: "wf-1",
		Mounts:     []MountSpec{{Source: filepath.Join(root, "proj"), Dest: "/work"}},
	}); err != nil {
		t.Fatalf("an absolute source under an allowed root was rejected: %v", err)
	}
}

// TestCreateMountArgvUsesHostPathsOnly is the argv half of BF-3(a): whatever the
// request carries, every `-v` source in the create argv is an ABSOLUTE host path
// — a bind — never a volume name, and the read-only marker is preserved.
func TestCreateMountArgvUsesHostPathsOnly(t *testing.T) {
	mounts := []MountSpec{
		{Source: "/home/op/proj", Dest: "/home/op/proj"},
		{Source: "/home/op/.config/opencode/opencode.json", Dest: "/home/op/.config/opencode/opencode.json", RO: true},
	}
	args := mountArgv(mounts)
	if len(args) != 2*len(mounts) {
		t.Fatalf("expected one -v pair per mount, got %v", args)
	}
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] != "-v" {
			t.Fatalf("argv is not -v/value shaped: %v", args)
		}
		src := strings.SplitN(args[i+1], ":", 2)[0]
		if !filepath.IsAbs(src) {
			t.Fatalf("mount argv carries the non-absolute source %q — that is a named volume or a relative "+
				"path, and both can outlive the pristine container reset (argv=%v)", src, args)
		}
	}
	if !strings.HasSuffix(args[3], ":ro") {
		t.Fatalf("a read-only mount lost its :ro marker: %q", args[3])
	}
}
