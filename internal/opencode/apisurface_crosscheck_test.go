package opencode

// apisurface_crosscheck_test.go — THE DAEMON'S PATHS AND THE CLIENT'S SHAPE TABLE MUST AGREE.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/runtime"
)

func TestTheDaemonGatePathsMatchTheShapeTable(t *testing.T) {
	health := runtime.OpenCodeHealthPaths()
	create := runtime.OpenCodeSessionCreatePaths()

	// v1 first: an install in the field must be answered on the first request, exactly as before.
	wantHealth := []string{shapeV1.healthPath, shapeV2.healthPath}
	wantCreate := []string{shapeV1.sessionsPath(), shapeV2.sessionsPath()}

	if len(health) != len(wantHealth) {
		t.Fatalf("the daemon probes %d health paths %v, but there are %d surfaces %v — a missing "+
			"entry means one generation's serve can never be reported ready",
			len(health), health, len(wantHealth), wantHealth)
	}
	for i := range wantHealth {
		if health[i] != wantHealth[i] {
			t.Errorf("daemon health path %d = %q, want %q (surface %s) — the gate and the client "+
				"would be probing different surfaces", i, health[i], wantHealth[i],
				[]string{"v1", "v2"}[i])
		}
	}
	if len(create) != len(wantCreate) {
		t.Fatalf("the daemon probes %d create paths %v, want %d %v", len(create), create,
			len(wantCreate), wantCreate)
	}
	for i := range wantCreate {
		if create[i] != wantCreate[i] {
			t.Errorf("daemon create path %d = %q, want %q (surface %s)", i, create[i], wantCreate[i],
				[]string{"v1", "v2"}[i])
		}
	}
}
