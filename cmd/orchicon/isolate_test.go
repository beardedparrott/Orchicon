package main

// isolate_test.go — TEST HERMETICITY for the serve command's package vars.

import "github.com/beardedparrott/orchicon/internal/testfixtures"

func init() {
	testfixtures.UnsetAmbientConfigEnv()
}
