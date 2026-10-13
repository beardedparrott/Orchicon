package permpolicy

// unwritable_dir_test.go — an unwritable DATA DIRECTORY is not an invalid policy.
//
// These pin the classification `cmd/orchicon/serve.go` depends on. That caller splits one
// failure into two messages:
//
//   - a MALFORMED file keeps "invalid permission policy" + the path + the parse error;
//   - an unwritable directory gets its own message naming the DATA DIR and the fix.
//
// The split is only real if it can distinguish them, and it discriminates on
// `errors.Is(err, fs.ErrPermission)`. If the error this package produces does not satisfy
// that, the second branch is dead code and the operator is back to being told their POLICY is
// invalid when the problem is a directory they cannot write — which is what happened: a bare
// `orchicon serve` on the host defaults DataDir to /var/lib/orchicon, which needs root, and
// the log said "invalid permission policy" about a file that may not even exist yet.
//
// So this asserts the MECHANISM (the error is recognisable as a permission failure) rather
// than the caller's prose, which is the part that cannot be checked from here.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unwritableParent returns a directory that cannot be written to, and the path of a
// subdirectory inside it that therefore cannot be created.
func unwritableParent(t *testing.T) (dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		// Root ignores the mode bits, so the condition cannot be provoked. Skipping is the
		// honest outcome: a passing test here would prove nothing about the classification.
		t.Skip("running as root: a read-only directory is still writable, so this cannot be provoked")
	}
	parent := t.TempDir()
	dir = filepath.Join(parent, "state")
	if err := os.Chmod(parent, 0o500); err != nil { // r-x: no write, so no mkdir underneath
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) }) // let TempDir clean up
	return dir
}

func TestAnUnwritableDataDirSatisfiesFSErrPermission(t *testing.T) {
	dir := unwritableParent(t)
	path := filepath.Join(dir, "permission-policy.yaml")

	err := Boot(path)
	if err == nil {
		t.Fatal("Boot must fail when the policy's directory cannot be created")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("the error must satisfy fs.ErrPermission — cmd/orchicon/serve.go discriminates on "+
			"exactly that to tell an unwritable DATA DIRECTORY apart from a malformed policy. If it "+
			"does not match, the data-dir branch is dead and the operator is told 'invalid permission "+
			"policy' about a file that is not at fault. Got: %v", err)
	}
}

func TestTheMkdirFailureIsNotSpelledTwice(t *testing.T) {
	dir := unwritableParent(t)
	path := filepath.Join(dir, "permission-policy.yaml")

	err := Boot(path)
	if err == nil {
		t.Fatal("Boot must fail when the policy's directory cannot be created")
	}
	msg := err.Error()

	// The message must name what went wrong...
	if !strings.Contains(msg, "cannot create "+dir) {
		t.Errorf("the message must name the directory and the operation, got: %q", msg)
	}
	// ...and must NOT repeat the syscall wrapper. The old text was
	// `permission policy: mkdir <dir>: mkdir <dir>: permission denied`, which reads as a bug —
	// *os.PathError already spells out the failing syscall and its path.
	if strings.Contains(msg, "mkdir "+dir+": mkdir "+dir) {
		t.Errorf("the failing syscall is reported twice, got: %q", msg)
	}
}

func TestAMalformedPolicyIsNotAPermissionFailure(t *testing.T) {
	// The other half of the split, and the reason the first branch cannot simply be "any error
	// from Boot": a genuinely malformed policy must NOT be classified as a permission problem,
	// or the loud "invalid permission policy" message — the one that stops a boot rather than
	// let an operator believe exclusions are in force — would never be reached.
	dir := t.TempDir()
	path := filepath.Join(dir, "permission-policy.yaml")
	if err := os.WriteFile(path, []byte("deny: [this is not valid: yaml: at all\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := Boot(path)
	if err == nil {
		t.Fatal("a malformed policy must fail the boot loudly")
	}
	if errors.Is(err, fs.ErrPermission) {
		t.Fatalf("a malformed policy must NOT be classified as a permission failure, or serve.go "+
			"would report it as a data-dir problem: %v", err)
	}
}
