package verify

// declaration_test.go — THE OLD FILE FORMAT KEEPS WORKING, and the new one is strict.
//
// The first test is the compatibility guarantee: every project that already has a one-line `.orchicon/verify`
// must behave identically after this change. The rest pin the opt-in.

import (
	"strings"
	"testing"
)

// AN EXISTING ONE-LINE FILE IS UNCHANGED — the compatibility guarantee.
func TestABareCommandStillMeansThatCommand(t *testing.T) {
	d, err := ParseDeclaration("go build ./... && go vet ./... && go test ./internal/... ./cmd/...\n")
	if err != nil {
		t.Fatalf("ParseDeclaration: %v", err)
	}
	if d.Command != "go build ./... && go vet ./... && go test ./internal/... ./cmd/..." {
		t.Errorf("Command = %q", d.Command)
	}
	if d.Format != FormatExitCode {
		t.Errorf("Format = %q, want exit_code — a bare command promises no result format", d.Format)
	}
	if d.PerCheck() {
		t.Error("a bare command claims to yield per-check results")
	}
}

// COMMENTS AND BLANK LINES ARE IGNORED in both forms, which is what makes the file readable.
func TestCommentsAreIgnored(t *testing.T) {
	d, err := ParseDeclaration("# the gate\n\nmake test\n\n# why: fast\n")
	if err != nil {
		t.Fatalf("ParseDeclaration: %v", err)
	}
	if d.Command != "make test" {
		t.Errorf("Command = %q, want make test", d.Command)
	}
}

// THE OPT-IN FORM CARRIES THE FORMAT.
func TestAKeyValueDeclaration(t *testing.T) {
	d, err := ParseDeclaration(`# our gate
command = go test -json ./... | gotestsum --junitfile report.xml
format  = junit
path    = report.xml
`)
	if err != nil {
		t.Fatalf("ParseDeclaration: %v", err)
	}
	if !strings.Contains(d.Command, "gotestsum") {
		t.Errorf("Command = %q", d.Command)
	}
	if d.Format != FormatJUnit {
		t.Errorf("Format = %q, want junit", d.Format)
	}
	if d.Path != "report.xml" {
		t.Errorf("Path = %q, want report.xml", d.Path)
	}
	if !d.PerCheck() {
		t.Error("a junit declaration must report per-check results")
	}
}

// A BARE COMMAND CONTAINING '=' IS NOT MISTAKEN FOR A KEY — `GOFLAGS=-count=1 go test ./...` is legal shell and
// its first token looks exactly like a key.
//
// THIS IS THE BUG THE KNOWN-KEY RULE EXISTS FOR: a parser that splits on the first '=' would read the key as
// "GOFLAGS" and either take `-count=1 go test ./...` as the COMMAND (dropping the real one) or report an
// unknown key. Both leave the project unmeasured while looking configured.
func TestAnEnvironmentPrefixIsNotAKey(t *testing.T) {
	d, err := ParseDeclaration("GOFLAGS=-count=1 go test ./...")
	if err != nil {
		t.Fatalf("ParseDeclaration: %v", err)
	}
	if d.Command != "GOFLAGS=-count=1 go test ./..." {
		t.Errorf("Command = %q; the whole line is the command", d.Command)
	}
	if d.Format != FormatExitCode {
		t.Errorf("Format = %q, want exit_code", d.Format)
	}
}

// AN UNKNOWN KEY IS AN ERROR. A typo must not silently disable measurement.
func TestAnUnknownKeyIsRejected(t *testing.T) {
	if _, err := ParseDeclaration("command = make test\nformats = junit\n"); err == nil {
		t.Fatal("an unknown key was accepted; the declaration would be silently ignored")
	}
}

// A BAD FORMAT IS AN ERROR, named at the point of reading.
func TestABadFormatIsRejected(t *testing.T) {
	if _, err := ParseDeclaration("command = make test\nformat = junitxmlplease\n"); err == nil {
		t.Fatal("an invalid format was accepted")
	}
}

// NO COMMAND IS AN ERROR in the key/value form: a file that sets only `format` declared nothing to run, and
// treating that as "no gate" would be indistinguishable from an empty file.
func TestAKeyValueFormWithoutACommandIsRejected(t *testing.T) {
	if _, err := ParseDeclaration("format = junit\n"); err == nil {
		t.Fatal("a declaration with a format but no command was accepted")
	}
}

// AN EMPTY FILE DECLARES NOTHING, which is legitimate (and is what most projects have).
func TestAnEmptyDeclaration(t *testing.T) {
	d, err := ParseDeclaration("# nothing here\n\n")
	if err != nil {
		t.Fatalf("ParseDeclaration: %v", err)
	}
	if d.Declared() {
		t.Error("an empty file was read as declaring a gate")
	}
}
