package runtime

// The engine contract tests: which engine the daemon defaults to, what an ABSENT
// engine reports (AC 2), and — flag by flag — where podman and docker differ.
//
// The flag differences cannot be verified by RUNNING an engine in CI (the image
// ships no podman/docker), so they are pinned here as the exact argv the daemon
// will emit, and the reasons live next to the code that emits them
// (engine.go, daemon.go createFlagArgs).

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// TestEngineDefaultsToPodman pins the default. Everything downstream (the pool
// key, the create argv, the image-label template) reads the SAME value, so the
// default lives in exactly one place.
func TestEngineDefaultsToPodman(t *testing.T) {
	if got := DefaultEngineName(); got != "podman" {
		t.Fatalf("DefaultEngineName() = %q, want podman", got)
	}
	// A zero-valued Engine is the DEFAULT engine, never "": a bare Daemon in a
	// test must not exec an empty binary name.
	var zero Engine
	if got := zero.name(); got != "podman" {
		t.Errorf("zero Engine name() = %q, want podman", got)
	}
	if got := zero.binary(); got != "podman" {
		t.Errorf("zero Engine binary() = %q, want podman", got)
	}
}

// TestResolveEngineRejectsUnknownName pins that a typo is REJECTED, not guessed.
func TestResolveEngineRejectsUnknownName(t *testing.T) {
	_, err := ResolveEngine("containerd")
	if err == nil {
		t.Fatal("an unknown engine must be an error, not a silent fallback")
	}
	for _, want := range []string{"containerd", "podman", "docker", EnvEngine} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name %q", err, want)
		}
	}
}

// TestResolveEngineDefault is the environment-tolerant half: it asserts the
// RESOLUTION CONTRACT (empty => the default engine, resolved to a real path) and
// tolerates a host with no engine installed by asserting the actionable error
// instead.
func TestResolveEngineDefault(t *testing.T) {
	eng, err := ResolveEngine("")
	if err != nil {
		var miss *EngineMissingError
		if !errors.As(err, &miss) {
			t.Fatalf("a missing default engine must be an *EngineMissingError, got %T: %v", err, err)
		}
		if miss.Name != DefaultEngineName() {
			t.Errorf("missing engine name = %q, want %q", miss.Name, DefaultEngineName())
		}
		t.Logf("no container engine on PATH in this environment (expected in CI): %v", err)
		return
	}
	if eng.Name != DefaultEngineName() {
		t.Errorf("resolved name = %q, want %q", eng.Name, DefaultEngineName())
	}
	if eng.Bin == "" {
		t.Error("a resolved engine must carry its binary path")
	}
}

// TestEngineMissingErrorIsActionable is AC 2: an absent engine names what to
// install and how — never a bare `exec: "podman": executable file not found`.
func TestEngineMissingErrorIsActionable(t *testing.T) {
	msg := Engine{Name: "podman", Bin: "podman"}.MissingError().Error()
	for _, want := range []string{
		"podman",
		"brew install podman",
		"podman machine init",
		"dnf install podman",
		"apt-get install podman",
		"winget",
		EnvEngine + "=docker",
		"runtime-daemon",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the podman-missing error must contain %q; got:\n%s", want, msg)
		}
	}
	// The underlying exec failure is wrapped, not the whole message.
	if !errors.Is(Engine{Name: "podman", Bin: "podman"}.MissingError(), exec.ErrNotFound) {
		t.Error("the missing-engine error must wrap exec.ErrNotFound")
	}

	dockerMsg := Engine{Name: "docker"}.MissingError().Error()
	for _, want := range []string{"Docker", "docker", EnvEngine} {
		if !strings.Contains(dockerMsg, want) {
			t.Errorf("the docker-missing error must contain %q; got:\n%s", want, dockerMsg)
		}
	}
}

// TestEngineFlagArgs pins the per-engine flag set (AC 3 / AC 6):
// --user is preserved for BOTH engines, --userns=keep-id and the loopback
// publish are podman-only, and the host alias is emitted for both.
func TestEngineFlagArgs(t *testing.T) {
	podman := Engine{Name: "podman", Bin: "podman"}
	docker := Engine{Name: "docker", Bin: "docker"}

	// AC 6: --userns=keep-id rides alongside --user so a rootless podman maps
	// the container uid onto the HOST uid (project dirs written as the
	// operator). Docker needs no equivalent.
	if got, want := podman.UsernsArgs(), []string{"--userns=keep-id"}; !reflect.DeepEqual(got, want) {
		t.Errorf("podman UsernsArgs = %v, want %v", got, want)
	}
	if got := docker.UsernsArgs(); got != nil {
		t.Errorf("docker UsernsArgs = %v, want none (--user is already the host uid)", got)
	}

	// ADR-0013 D2.1: podman publishes on the host loopback; docker keeps the
	// bridge IP (no publish at all).
	if got, want := podman.PublishArgs(defaultServePort, sandboxPlanePort),
		[]string{"-p", "127.0.0.1::4096", "-p", "127.0.0.1::8080"}; !reflect.DeepEqual(got, want) {
		t.Errorf("podman PublishArgs = %v, want %v", got, want)
	}
	if got := docker.PublishArgs(defaultServePort, sandboxPlanePort); got != nil {
		t.Errorf("docker PublishArgs = %v, want none", got)
	}
	// A zero/negative port is a programming error, not something to publish.
	if got := podman.PublishArgs(0, -1); len(got) != 0 {
		t.Errorf("PublishArgs must skip non-positive ports, got %v", got)
	}

	// D2.4: ONE host alias spelling on both engines and all platforms.
	for _, e := range []Engine{podman, docker} {
		if !argValue(e.CreateBaseArgs("rt-1", "wf1", "dev"), "--add-host", "host.docker.internal:host-gateway") {
			t.Errorf("%s must emit --add-host host.docker.internal:host-gateway", e.name())
		}
		if !argValue(e.CreateBaseArgs("rt-1", "wf1", "dev"), "--label", "orchicon.workflow=wf1") {
			t.Errorf("%s must label the container with its workflow", e.name())
		}
	}

	// The image-label template DIFFERS: docker nests labels under the image
	// config, podman exposes them at the top level. Getting this wrong does not
	// error — it silently rejects every custom image.
	if got := podman.ImageLabelFormat("org.orchicon.runtime-base"); got != `{{index .Labels "org.orchicon.runtime-base"}}` {
		t.Errorf("podman ImageLabelFormat = %q", got)
	}
	if got := docker.ImageLabelFormat("org.orchicon.runtime-base"); got != `{{index .Config.Labels "org.orchicon.runtime-base"}}` {
		t.Errorf("docker ImageLabelFormat = %q", got)
	}
}

// TestEngineNameInUseTokens pins the retry predicate against the REAL wordings
// (ADR-0013 D5.2) — podman's phrasings are not covered by docker's, so an
// unadjusted predicate would fail creates that used to retry.
func TestEngineNameInUseTokens(t *testing.T) {
	podman := Engine{Name: "podman"}
	docker := Engine{Name: "docker"}

	for _, out := range []string{
		`Error: creating container storage: the container name "orchicon-rt-1" is already in use by 3f2a`,
		`Error: container orchicon-rt-1 is in use`,
	} {
		if !nameInUse(out, podman.NameInUseTokens()) {
			t.Errorf("podman retry predicate must cover %q", out)
		}
	}
	if nameInUse("permission denied while trying to connect", podman.NameInUseTokens()) {
		t.Error("a real failure must not be treated as a name conflict")
	}

	for _, out := range []string{
		`Conflict. The container name "/orchicon-rt-1" is already in use by container "abc"`,
		`removal of container abc is already in progress`,
	} {
		if !nameInUse(out, docker.NameInUseTokens()) {
			t.Errorf("docker retry predicate must cover %q", out)
		}
	}
}

// TestEngineHostEndpoint pins AC 4's resolution for both engines: podman parses
// the published loopback port, docker reports the container IP and container
// port (unchanged).
func TestEngineHostEndpoint(t *testing.T) {
	podman := Engine{Name: "podman"}
	var gotArgs []string
	host, port, err := podman.HostEndpoint("rt-1", defaultServePort, func(args ...string) (string, error) {
		gotArgs = args
		return "127.0.0.1:32987\n", nil
	})
	if err != nil {
		t.Fatalf("podman HostEndpoint: %v", err)
	}
	if host != "127.0.0.1" || port != 32987 {
		t.Errorf("podman HostEndpoint = %s:%d, want 127.0.0.1:32987", host, port)
	}
	if want := []string{"port", "rt-1", "4096/tcp"}; !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("podman address argv = %v, want %v", gotArgs, want)
	}

	// A wildcard/dual-stack binding is treated as loopback rather than failing.
	if host, port, err := podman.HostEndpoint("rt-1", defaultServePort, func(args ...string) (string, error) {
		return "0.0.0.0:32768\n[::]:32768\n", nil
	}); err != nil || host != "127.0.0.1" || port != 32768 {
		t.Errorf("wildcard publish parsed to %s:%d err=%v, want 127.0.0.1:32768", host, port, err)
	}

	// Nothing published is a LOUD error — a serve the plane cannot dial must not
	// be reported as reachable.
	if _, _, err := podman.HostEndpoint("rt-1", defaultServePort, func(args ...string) (string, error) {
		return "", nil
	}); err == nil {
		t.Error("an empty `port` answer must be an error")
	}

	docker := Engine{Name: "docker"}
	host, port, err = docker.HostEndpoint("rt-1", defaultServePort, func(args ...string) (string, error) {
		return "172.17.0.5\n", nil
	})
	if err != nil {
		t.Fatalf("docker HostEndpoint: %v", err)
	}
	if host != "172.17.0.5" || port != defaultServePort {
		t.Errorf("docker HostEndpoint = %s:%d, want 172.17.0.5:%d", host, port, defaultServePort)
	}
}

// TestResolveEngineFromEnvHonoursTheProjectWideVariable pins the env precedence:
// ORCHICON_RUNTIME_ENGINE (the ADR's spelling) wins, ORCHICON_ENGINE — which
// scripts/lib/engine.sh, scripts/runtime.sh and the Makefile already read — is
// honoured as a FALLBACK, and neither set means the default engine. The point is
// that one host cannot build/pull its runtime images with one engine and create
// containers with the other.
func TestResolveEngineFromEnvHonoursTheProjectWideVariable(t *testing.T) {
	cases := []struct {
		name          string
		runtimeEngine string
		projectWide   string
		want          string
	}{
		{name: "neither set", want: DefaultEngineName()},
		{name: "project-wide only", projectWide: "docker", want: "docker"},
		{name: "ADR spelling only", runtimeEngine: "podman", want: "podman"},
		{name: "ADR spelling wins", runtimeEngine: "docker", projectWide: "podman", want: "docker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvEngine, tc.runtimeEngine)
			t.Setenv(EnvEngineAlias, tc.projectWide)
			eng, err := ResolveEngineFromEnv()
			if err != nil {
				// No engine binary in CI: the error must still name the engine the
				// environment selected, which is what proves the precedence.
				var miss *EngineMissingError
				if !errors.As(err, &miss) {
					t.Fatalf("a missing engine must be an *EngineMissingError, got %T: %v", err, err)
				}
				if miss.Name != tc.want {
					t.Errorf("missing engine = %q, want %q (environment precedence)", miss.Name, tc.want)
				}
				return
			}
			if eng.Name != tc.want {
				t.Errorf("resolved engine = %q, want %q", eng.Name, tc.want)
			}
		})
	}

	// A typo in the project-wide variable is REJECTED, not silently defaulted:
	// a typo must not leave the daemon on podman while the operator believes it
	// selected something else.
	t.Setenv(EnvEngine, "")
	t.Setenv(EnvEngineAlias, "containerd")
	if _, err := ResolveEngineFromEnv(); err == nil {
		t.Error("an unknown project-wide engine name must be an error")
	}
}
