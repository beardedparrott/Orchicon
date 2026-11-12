package runtime

// probe_argv_test.go — THE ACCEPTANCE PROBE for the --tmpfs grammar fix.

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestProbeEmitCreateArgv(t *testing.T) {
	if os.Getenv("ORCHICON_PROBE_ARGV") != "1" {
		t.Skip("set ORCHICON_PROBE_ARGV=1 to emit the create argv for an engine probe")
	}
	home := os.Getenv("HOME")
	d, _ := podmanCreateDaemon(t)
	d.Engine = Engine{Name: "podman", Bin: "podman"}
	d.HostHome = home
	d.Image = "orchicon-runtime:orchicon-dev"

	args := d.createFlagArgs("__PROBE_NAME__", CreateRequest{WorkflowID: "probe", InstanceID: "dev"})
	args = append(args, d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode"}, GitStrategy: "local"})...)
	args = append(args, d.Image, "bash", "-c", "ls -ld $HOME; touch $HOME/.probe && echo HOME_WRITEABLE")

	// ONE ARG PER LINE, so the driver can read them back verbatim without a shell
	// re-splitting a path that contains a space.
	fmt.Println(strings.Join(args, "\n"))
}
