package main

// lifecycle_test.go — `orchicon start` / `orchicon stop`, and the ONE rule every
// lifecycle verb now shares: the instance it acts on comes from its ARGUMENTS, not
// only from the environment.
//
// THE DEFECT THESE PIN. `runInstall` and `runRestart` accepted `args` and never read
// them, so the instance came from ORCHICON_INSTANCE alone:
//
//	orchicon install --instance prod   → installed/repaired DEV
//	orchicon restart --instance prod   → restarted DEV
//
// Nothing failed. USERGUIDE.md documented `orchicon restart --instance <dev|test|prod>`
// as if it worked, so an operator who believed they had chosen an instance had not, and
// a restart is the worst case for that: both instances come back HEALTHY, and the one
// that was meant to pick up a new PATH kept its old environment. A test that only ever
// exercised the env var could never see it — which is why these call the parser with
// ARGUMENTS and assert what comes back.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readSource reads a file from this package's directory, for the source-level pins
// below. The package's other source pins read container.go through readContainerGo;
// these need more than one file, so they take the name.
func readSource(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return b
}

func TestParseInstanceTarget(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		fallback string
		want     string
		wantErr  string
	}{
		// The forms the docs promised, and the habit relied on.
		{name: "no argument falls back", args: nil, fallback: "", want: "dev"},
		{name: "no argument uses ORCHICON_INSTANCE", args: nil, fallback: "prod", want: "prod"},
		{name: "positional", args: []string{"prod"}, want: "prod"},
		{name: "positional overrides the fallback", args: []string{"test"}, fallback: "prod", want: "test"},
		{name: "--instance flag", args: []string{"--instance", "prod"}, want: "prod"},
		{name: "-instance flag", args: []string{"-instance", "test"}, want: "test"},
		{name: "--instance= form", args: []string{"--instance=prod"}, want: "prod"},
		{name: "flag overrides the fallback", args: []string{"--instance", "test"}, fallback: "prod", want: "test"},
		{name: "positional agreeing with the flag", args: []string{"prod", "--instance", "prod"}, want: "prod"},

		// Refusals. Every one of these used to be either ignored or silently misread.
		{
			name: "unknown flag is refused, not ignored",
			args: []string{"--frobnicate"}, wantErr: "unknown flag",
		},
		{
			name: "a flag whose value is missing is refused",
			args: []string{"--instance"}, wantErr: "needs a value",
		},
		{
			name: "two positionals are ambiguous",
			args: []string{"dev", "prod"}, wantErr: "two instances given",
		},
		{
			name: "a positional that disagrees with the flag is ambiguous",
			args: []string{"dev", "--instance", "prod"}, wantErr: "name one",
		},
		{
			name: "an empty value is refused",
			args: []string{"--instance", ""}, wantErr: "may not be empty",
		},
		// The path guard, which used to be three separate copies.
		{
			name: "a traversal name is refused",
			args: []string{"../../etc"}, wantErr: "may not contain a path separator",
		},
		{
			name: "a name with a separator is refused",
			args: []string{"dev/prod"}, wantErr: "may not contain a path separator",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseInstanceTarget(c.args, c.fallback)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("args %v: no error; got instance %q, want an error containing %q",
						c.args, got, c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("args %v: error %q does not contain %q", c.args, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("args %v: unexpected error: %v", c.args, err)
			}
			if got != c.want {
				t.Errorf("args %v: got instance %q, want %q", c.args, got, c.want)
			}
		})
	}
}

// THE REGRESSION ITSELF, stated as the two commands that lied. Each verb's instance
// resolution is exercised through the parser with the argument an operator would
// actually type, because "the parser works" is not the claim — "install/restart/start/
// stop honour it" is.
func TestLifecycleVerbsHonourTheInstanceArgument(t *testing.T) {
	// Every verb that resolves an instance from args, with the argv shape the user types.
	// `plane` and `services` pass their tail (the verb is args[0], consumed by the caller).
	verbs := map[string][]string{
		"install":  {"--instance", "prod"},
		"restart":  {"--instance", "prod"},
		"start":    {"--instance", "prod"},
		"stop":     {"--instance", "prod"},
		"plane":    {"prod"},
		"services": {"prod"},
	}
	for verb, args := range verbs {
		t.Run(verb, func(t *testing.T) {
			// The environment deliberately says something ELSE. If a verb still preferred the
			// env var, this would return "test" and the assertion below would fail — which is
			// exactly how the original defect would have shown up.
			t.Setenv("ORCHICON_INSTANCE", "test")
			got, err := parseInstanceTarget(args, env("ORCHICON_INSTANCE", "dev"))
			if err != nil {
				t.Fatalf("%s %v: unexpected error: %v", verb, args, err)
			}
			if got != "prod" {
				t.Fatalf("%s %v acted on instance %q, not %q — the argument was ignored, which is the\n"+
					"defect this parity exists to prevent (the env var wins unless args are read)",
					verb, args, got, "prod")
			}
		})
	}
}

// The env var is still the fallback, so an existing script that exports
// ORCHICON_INSTANCE keeps working unchanged. A fix that broke that would be a regression
// in the other direction.
func TestLifecycleFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("ORCHICON_INSTANCE", "prod")
	got, err := parseInstanceTarget(nil, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "prod" {
		t.Errorf("ORCHICON_INSTANCE=prod was ignored: got %q", got)
	}
}

// A SOURCE-LEVEL PIN on the thing that actually broke: the two verbs that ignored their
// arguments must call the parser. This is deliberately not behavioural — `runInstall`
// needs an engine, a network and a database to reach the point where the instance is
// observable, so a behavioural test of it is not something CI can run. The failure being
// pinned is "the parameter is never read", and that is visible in the source.
func TestLifecycleVerbsParseTheirArguments(t *testing.T) {
	for _, tc := range []struct{ file, fn string }{
		{"install.go", "func runInstall(args []string, log *slog.Logger) error {"},
		{"restart.go", "func runRestart(args []string, log *slog.Logger) error {"},
		{"lifecycle.go", "func runStart(args []string, log *slog.Logger) error {"},
		{"lifecycle.go", "func runStop(args []string) error {"},
	} {
		t.Run(tc.fn, func(t *testing.T) {
			body := functionBody(t, tc.file, tc.fn)
			if !strings.Contains(body, "parseInstanceTarget(") {
				t.Fatalf("%s does not call parseInstanceTarget — its `args` parameter is accepted and\n"+
					"not honoured, which is how `--instance prod` came to act on dev", tc.fn)
			}
			// The env read must go THROUGH the parser as its fallback, never be assigned directly.
			if strings.Contains(body, `instance := env("ORCHICON_INSTANCE"`) {
				t.Fatalf("%s still reads ORCHICON_INSTANCE directly instead of passing it to\n"+
					"parseInstanceTarget as the fallback, so an argument can never win", tc.fn)
			}
		})
	}
}

// functionBody returns one function's source, from its signature to the closing brace at
// column 0. It stops at the next top-level `func ` so a match cannot run on into a
// sibling and pass on that sibling's content.
func functionBody(t *testing.T, file, signature string) string {
	t.Helper()
	src := string(readSource(t, file))
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("%s: %q not found", file, signature)
	}
	body := src[start:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}
	return body
}

// THE LEGACY-INSTANCE CASE, which the first version of these verbs got wrong on the
// operator's OWN host. `ports.json` and the residency record are written only by the
// installer that shipped with #729/#730; both instances on this machine were installed
// before that, have neither, and yet their planes run and serve. Keying "is it
// installed?" on the record made `orchicon stop dev` report "nothing is installed for
// it" — a successful no-op that stopped nothing, which is the exact failure mode these
// verbs exist to remove.
//
// So the evidence is the DATA DIRECTORY, tested here on both sides: absent means nothing
// to stop, present means stop it, and neither case consults a record.
func TestInstanceExistsUsesTheDataDirectoryNotTheInstallRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Nothing installed: no data dir, and in particular no record either.
	if instanceExists("prod") {
		t.Fatal("instanceExists(prod) = true before any data directory exists")
	}
	if _, ok := recordedResidency("prod"); ok {
		t.Fatal("this test's premise is that there is no residency record")
	}

	// A data dir alone — no ports.json, no residency record. This is exactly what a
	// pre-#729 install looks like on disk.
	dir := filepath.Join(home, ".local", "share", "orchicon-prod")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !instanceExists("prod") {
		t.Fatal("instanceExists(prod) = false with a data directory present — a legacy install\n" +
			"would be told there is nothing to stop while its plane is running")
	}
	// Still no record, which is the point: the answer did not depend on one.
	if _, ok := recordedResidency("prod"); ok {
		t.Fatal("something wrote a residency record; this test no longer proves the point")
	}

	// And the residency default for a record-free instance is HOST, never container —
	// guessing "container" would run an engine command against a container these verbs
	// do not own.
	if got := instanceResidency("prod"); got != residencyHost {
		t.Errorf("instanceResidency(prod) with no record = %q, want %q", got, residencyHost)
	}
}
