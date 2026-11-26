package runtime

import (
	"os"
	"strings"
	"testing"
)

// NO MEMORY LIMIT IS THE DEFAULT. A 4 GiB cap, with a 2 GiB /tmp tmpfs inside it, made a run on a 121 GB host
// hit its own limit 15,764 times and then fail. "0" and "" both mean unlimited, and the flag is OMITTED so no
// engine has to interpret a zero.
func TestUnlimitedMemoryOmitsTheEngineFlag(t *testing.T) {
	for _, mem := range []string{"0", "", "  "} {
		d, _ := podmanCreateDaemon(t)
		d.Memory = mem
		args := d.createFlagArgs("orchicon-rt-1", CreateRequest{WorkflowID: "wf-1"})
		for _, a := range args {
			if a == "--memory" {
				t.Errorf("Memory=%q still passed --memory: %v", mem, args)
			}
		}
	}
	// An operator who WANTS a ceiling still gets one.
	d, _ := podmanCreateDaemon(t)
	d.Memory = "16g"
	if args := d.createFlagArgs("orchicon-rt-1", CreateRequest{WorkflowID: "wf-1"}); !argValue(args, "--memory", "16g") {
		t.Errorf("an explicit ORCHICON_RUNTIME_MEMORY=16g was not applied: %v", args)
	}
}

// The default is a property of the CLI wiring, so it is pinned there: a change back to a number is a change
// to this decision and must be made on purpose.
func TestTheDefaultRuntimeMemoryIsUnlimited(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/orchicon/runtime.go")
	if err != nil {
		t.Fatalf("read runtime.go: %v", err)
	}
	if !strings.Contains(string(raw), `env("ORCHICON_RUNTIME_MEMORY", "0")`) {
		t.Error(`ORCHICON_RUNTIME_MEMORY no longer defaults to "0" (no limit) — the default is meant to be unlimited`)
	}
}

// THE SESSION STORE HAS ITS OWN MOUNT. With /tmp full a SQLite write in the store directory fails ("database
// or disk is full" — opencode's "Failed to execute statement") when the directory is part of /tmp, and
// succeeds when it is its own tmpfs (measured with a 64 MiB /tmp in a throwaway container).
func TestSessionStoreHasItsOwnTmpfs(t *testing.T) {
	d, _ := podmanCreateDaemon(t)
	d.StoreTmpfsSize = "1g"
	args := d.createFlagArgs("orchicon-rt-1", CreateRequest{WorkflowID: "wf-1"})

	var tmpfs []string
	for i, a := range args {
		if a == "--tmpfs" && i+1 < len(args) {
			tmpfs = append(tmpfs, args[i+1])
		}
	}
	storeAt, tmpAt := -1, -1
	for i, v := range tmpfs {
		switch {
		case strings.HasPrefix(v, serveDataDir+":"):
			storeAt = i
			if !strings.Contains(v, "size=1g") {
				t.Errorf("the store tmpfs is not sized by StoreTmpfsSize: %q", v)
			}
		case strings.HasPrefix(v, "/tmp:"):
			tmpAt = i
		}
	}
	if storeAt < 0 {
		t.Fatalf("no --tmpfs for %s: %v", serveDataDir, tmpfs)
	}
	if tmpAt < 0 || storeAt < tmpAt {
		t.Errorf("the store mount must come AFTER /tmp's so it nests inside it (store=%d, tmp=%d): %v", storeAt, tmpAt, tmpfs)
	}

	// Unset or "0" turns it off (an engine that cannot nest tmpfs mounts keeps the old shape).
	for _, off := range []string{"", "0"} {
		d.StoreTmpfsSize = off
		for _, v := range tmpfsValues(d.createFlagArgs("orchicon-rt-1", CreateRequest{WorkflowID: "wf-1"})) {
			if strings.HasPrefix(v, serveDataDir+":") {
				t.Errorf("StoreTmpfsSize=%q still mounted the store: %q", off, v)
			}
		}
	}
}

// The store directory the mount covers MUST be the one the serve actually writes: if they drift, the mount
// protects an empty directory and the store goes back to sharing /tmp.
func TestTheStoreMountCoversTheServeDataDir(t *testing.T) {
	if serveDataDir != "/tmp/orchicon-serve-data" {
		t.Fatalf("serveDataDir = %q; the store mount, the supervisor and the docs all name /tmp/orchicon-serve-data", serveDataDir)
	}
	raw, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "serveDataDir") {
		t.Error("agent.go no longer uses serveDataDir for the serve's XDG_DATA_HOME")
	}
}

func tmpfsValues(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--tmpfs" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}
