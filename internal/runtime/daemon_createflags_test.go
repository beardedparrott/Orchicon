package runtime

// AC 3 / AC 6 pinned as argv. The daemon cannot RUN an engine in CI, so the
// proof that every flag was verified is the argv itself: each element below is
// the element that was checked against both engines' CLIs, with the verdict
// recorded next to the code that emits it (daemon.go createFlagArgs, engine.go).
//
// daemon_secrets_test.go covers the other half — that createContainer sends
// THESE args plus its mounts/secrets/exe tail as one real `run` argv.

import (
	"reflect"
	"strings"
	"testing"
)

// podmanCreateDaemon is a daemon wired to the podman engine with the same
// resource values the CLI passes in production.
func podmanCreateDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()
	home, d := bootProfileTestHome(t)
	d.Engine = Engine{Name: "podman", Bin: "podman"}
	d.UserID, d.GroupID = 1000, 1000
	d.CPUs, d.Memory = "4", "4g"
	d.TmpfsSize, d.HomeTmpfsSize = "2g", "2g"
	return d, home
}

// TestCreateContainerFlagsPodman is the podman argv: every flag preserved from
// the docker create, plus the two podman-only additions (keep-id, loopback
// publish), and --user unchanged (AC 6).
func TestCreateContainerFlagsPodman(t *testing.T) {
	d, home := podmanCreateDaemon(t)
	args := d.createFlagArgs("orchicon-rt-1", CreateRequest{WorkflowID: "wf-1", InstanceID: "dev"})

	// Identity + ownership marker.
	if got, want := args[:6], []string{"run", "-d", "--name", "orchicon-rt-1", "--label", "orchicon.workflow=wf-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("create argv head = %v, want %v", got, want)
	}
	if !argValue(args, "--label", "orchicon.instance=dev") {
		t.Errorf("missing the instance label: %v", args)
	}

	// ADR-0013 D2.4: one host alias spelling, every engine, every platform.
	if !argValue(args, "--add-host", "host.docker.internal:host-gateway") {
		t.Errorf("missing --add-host host.docker.internal:host-gateway: %v", args)
	}
	// AC 6: --user is PRESERVED so bind-mounted projects are written as the
	// operator, never as root.
	if !argValue(args, "--user", "1000:1000") {
		t.Errorf("missing --user 1000:1000: %v", args)
	}
	// …and keep-id is what makes that uid resolve to the HOST uid.
	if !argvHas(args, "--userns=keep-id") {
		t.Errorf("missing --userns=keep-id (rootless podman would map --user into the subuid range): %v", args)
	}

	// Resources.
	if !argValue(args, "--cpus", "4") {
		t.Errorf("missing --cpus 4: %v", args)
	}
	if !argValue(args, "--memory", "4g") {
		t.Errorf("missing --memory 4g: %v", args)
	}
	// The private /tmp must stay mountable-exec: Go's TMPDIR and the
	// /tmp/orchicon-guard-* shim live there.
	if !argValue(args, "--tmpfs", "/tmp:rw,size=2g,exec") {
		t.Errorf("missing --tmpfs /tmp:rw,size=2g,exec: %v", args)
	}

	// ADR-0013 D2.1: the serve and sandbox-plane ports are published on the host
	// loopback, because a rootless container IP is not host-routable.
	for _, want := range []string{"127.0.0.1::4096", "127.0.0.1::8080"} {
		if !argValue(args, "-p", want) {
			t.Errorf("missing -p %s: %v", want, args)
		}
	}

	// The flag block carries no bind at all — every -v comes from the validated
	// mount set (standardHostMountArgs and the request's own mounts), so a
	// positionally-misplaced -v cannot hide in here.
	if argvHas(args, "-v") {
		t.Errorf("createFlagArgs must not emit -v: %v", args)
	}

	// The full create argv's uid-owned home tmpfs entries survive unchanged
	// (they are what make $HOME writable by the runtime uid).
	full := append(args, d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode"}, GitStrategy: "local"})...)
	for _, opt := range homeOwnedTmpfsOpts(home) {
		opt = strings.ReplaceAll(opt, "uid=0,gid=0", "uid=1000,gid=1000")
		if strings.HasPrefix(opt, home+":") {
			// $HOME itself carries the configured size; .local/.config never do.
			opt += ",size=2g"
		}
		if !argValue(full, "--tmpfs", opt) {
			t.Errorf("missing uid-owned home tmpfs %s: %v", opt, full)
		}
	}
}

// TestCreateContainerFlagsDocker pins the GRANDFATHERED docker path so the swap
// cannot silently change it: no publish, no keep-id, and the same preserved
// flags.
func TestCreateContainerFlagsDocker(t *testing.T) {
	d, _ := podmanCreateDaemon(t)
	d.Engine = Engine{Name: "docker", Bin: "docker"}
	args := d.createFlagArgs("orchicon-rt-1", CreateRequest{WorkflowID: "wf-1"})

	if argvHas(args, "-p") {
		t.Errorf("docker keeps the bridge-IP path and must not publish ports: %v", args)
	}
	if argvHas(args, "--userns=keep-id") {
		t.Errorf("docker has no keep-id (--user is already the host uid): %v", args)
	}
	if !argValue(args, "--user", "1000:1000") {
		t.Errorf("docker create must preserve --user: %v", args)
	}
	if !argValue(args, "--add-host", "host.docker.internal:host-gateway") {
		t.Errorf("docker create must keep the host alias: %v", args)
	}
	if !argValue(args, "--tmpfs", "/tmp:rw,size=2g,exec") {
		t.Errorf("docker create must keep the exec /tmp: %v", args)
	}
	// A request with no workflow id is the pool's warm-container create.
	if !argValue(d.createFlagArgs("orchicon-rt-1", CreateRequest{}), "--label", "orchicon.workflow=pool") {
		t.Errorf("an unlabelled request must label as pool: %v", args)
	}
}

// TestHostBaseURL pins AC 4's addressing end to end at the daemon seam: podman
// resolves the published loopback port (ServePort must be the HOST port —
// lifecycle.go and the opencode adapter build http://127.0.0.1:<ServePort> from
// it), docker keeps the container IP and its own port.
func TestHostBaseURL(t *testing.T) {
	podman := &Daemon{Engine: Engine{Name: "podman", Bin: "podman"}}
	podman.engineFn = func(args ...string) (string, error) {
		if len(args) == 3 && args[0] == "port" {
			return "127.0.0.1:32987\n", nil
		}
		return "", &fakeErr{"unexpected " + strings.Join(args, " ")}
	}
	base, port, err := podman.hostBaseURL("rt-1", defaultServePort)
	if err != nil {
		t.Fatalf("podman hostBaseURL: %v", err)
	}
	if base != "http://127.0.0.1:32987" || port != 32987 {
		t.Errorf("podman hostBaseURL = %s (port %d), want http://127.0.0.1:32987 (port 32987)", base, port)
	}

	docker := &Daemon{Engine: Engine{Name: "docker", Bin: "docker"}}
	docker.engineFn = func(args ...string) (string, error) { return "172.17.0.5\n", nil }
	base, port, err = docker.hostBaseURL("rt-1", defaultServePort)
	if err != nil {
		t.Fatalf("docker hostBaseURL: %v", err)
	}
	if base != "http://172.17.0.5:4096" || port != defaultServePort {
		t.Errorf("docker hostBaseURL = %s (port %d), want http://172.17.0.5:4096 (port 4096)", base, port)
	}
}

// contains reports whether args holds the exact element.
func argvHas(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
