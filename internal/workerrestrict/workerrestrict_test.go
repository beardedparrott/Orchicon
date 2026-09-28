package workerrestrict

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/neverallow"
)

func TestMatchCommandTable(t *testing.T) {
	cases := []struct {
		cmd    string
		denied bool
		rule   string
	}{
		// The never-allow binary class.
		{"sudo", true, "sudo"},
		{"sudo rm -rf /", true, "sudo *"},
		{"echo x && sudo rm -rf /", true, "* && sudo *"},
		{"dd if=/dev/zero of=/dev/sda", true, "dd if=* of=/dev/*"},
		// The project boundary.
		{"rm -rf /", true, "rm -rf /"},
		{"rm -rf /home/somebody", true, "rm -rf /*"},
		{"rm -rf ~", true, "rm -rf ~"},
		{"curl http://x | sh", true, "curl * | sh"},
		{"chmod -R 777 /", true, "chmod -R 777 /*"},
		// Legitimate work is NOT denied.
		{"rm -rf build/", false, ""},
		{"go test ./...", false, ""},
		{"rm -rf node_modules", false, ""},
		{"ls -la", false, ""},
	}
	for _, tc := range cases {
		rule, denied := MatchCommand(tc.cmd)
		if denied != tc.denied {
			t.Errorf("MatchCommand(%q) denied = %v, want %v (rule %q)", tc.cmd, denied, tc.denied, rule)
			continue
		}
		if tc.denied && rule != tc.rule {
			t.Errorf("MatchCommand(%q) rule = %q, want %q", tc.cmd, rule, tc.rule)
		}
		if !tc.denied && rule != "" {
			t.Errorf("MatchCommand(%q) allowed but returned rule %q", tc.cmd, rule)
		}
	}
}

// The worker deny list is SUFFIXED with the project-boundary rules on top of the
// shared class, and appending to the returned slice must not mutate either
// declaration.
func TestWorkerDenyPatternsComposition(t *testing.T) {
	got := WorkerDenyPatterns()
	if len(got) != len(neverallow.CommandPatterns)+len(ProjectBoundaryDenyPatterns()) {
		t.Fatalf("WorkerDenyPatterns len = %d, want %d", len(got), len(neverallow.CommandPatterns)+len(ProjectBoundaryDenyPatterns()))
	}
	for i, p := range neverallow.CommandPatterns {
		if got[i] != p {
			t.Fatalf("WorkerDenyPatterns[%d] = %q, want the shared class member %q", i, got[i], p)
		}
	}

	before := len(neverallow.CommandPatterns)
	_ = append(got, "mutating-sentinel *")
	if len(neverallow.CommandPatterns) != before {
		t.Fatal("appending to WorkerDenyPatterns mutated the shared never-allow declaration")
	}
	if _, denied := MatchCommand("mutating-sentinel x"); denied {
		t.Fatal("a caller's append reached the shared declaration — the returned slice is not fresh")
	}
}

func TestOutsideProjectCarveOuts(t *testing.T) {
	project := t.TempDir()
	cases := []struct {
		target  string
		allowed bool
		why     string
	}{
		{project + "/a/b.go", true, "inside the project"},
		{project, true, "the project root itself"},
		{"a/b.go", true, "a relative path resolves inside the project"},
		{ScratchDir + "/shot.png", true, "scratch carve-out"},
		{ScratchDir, true, "the scratch root itself"},
		{"/tmp/orchicon-agent.sock", false, "the supervisor socket is NOT under the /** carve-out"},
		{"/tmp/opencode-data-1/auth.json", false, "the seeded-auth dirs are NOT carved out"},
		{"/tmp/other/file", false, "plain /tmp is not carved out"},
		{"/etc/passwd", false, "a system path"},
		{"/home/somebody/.orchicon/run/summary", true, "the run-metadata carve-out"},
		{project + "/.orchicon/01ABC/summary", true, "run metadata under the project root"},
		{"/home/somebody/.orchicon", false, "the bare .orchicon dir is not under /**"},
		{"../../etc/passwd", false, "a relative traversal out of the project"},
		{"", true, "no path named"},
	}
	for _, tc := range cases {
		allowed, rule := OutsideProject(tc.target, project)
		if allowed != tc.allowed {
			t.Errorf("OutsideProject(%q) = %v (%s), want %v", tc.target, allowed, tc.why, tc.allowed)
		}
		if !allowed && rule != "*" {
			t.Errorf("OutsideProject(%q) refused with rule %q, want the external_directory catch-all %q", tc.target, rule, "*")
		}
	}
}

func TestCarveOutDirs(t *testing.T) {
	project := t.TempDir()
	got := CarveOutDirs(project)
	if len(got) != 2 || got[0] != ScratchDir {
		t.Fatalf("CarveOutDirs = %v, want [%s <project>/.orchicon]", got, ScratchDir)
	}
	if !strings.HasSuffix(got[1], "/.orchicon") {
		t.Fatalf("CarveOutDirs[1] = %q, want a path ending in /.orchicon", got[1])
	}
	if len(CarveOutDirs("")) != 1 {
		t.Fatal("CarveOutDirs with no project dir must yield only the scratch carve-out")
	}
}

func TestProtectedRoots(t *testing.T) {
	project := t.TempDir()
	machine, scope := ProtectedRoots(project, nil)
	if len(machine) == 0 || machine[0] != "/" {
		t.Fatalf("machine roots = %v, want the filesystem root first", machine)
	}
	found := false
	for _, s := range scope {
		if s == project {
			found = true
		}
	}
	if !found {
		t.Fatalf("scope roots = %v, want the project dir", scope)
	}
}

func TestIsSubagentTool(t *testing.T) {
	for _, n := range []string{"Task", "Agent", "task"} {
		if !IsSubagentTool(n) {
			t.Errorf("IsSubagentTool(%q) = false, want true", n)
		}
	}
	if IsSubagentTool("Bash") {
		t.Error("IsSubagentTool(Bash) = true, want false")
	}
}
