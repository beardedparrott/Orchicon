package askorchicon

// isolate_test.go — TEST HERMETICITY for the execution guard's profile switches.

import "github.com/beardedparrott/orchicon/internal/testfixtures"

func init() {
	testfixtures.UnsetAmbientConfigEnv()
}
