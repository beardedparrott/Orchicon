package runtime

import (
	"strings"
	"testing"
)

// An oversize serve config used to surface as "fork/exec …/opencode: argument list too long" — naming
// neither the variable nor its size. The daemon now refuses it up front with the number, before any exec.
func TestStartServeRefusesOversizeConfigWithAClearError(t *testing.T) {
	d := &Daemon{}
	_, _, _, err := d.startServe("c", "opencode", CreateRequest{ServeConfig: strings.Repeat("a", maxServeConfigBytes+1)})
	if err == nil {
		t.Fatal("want an error for a config over the single-env-string limit")
	}
	for _, want := range []string{"serve config is", "MAX_ARG_STRLEN", "files"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}
