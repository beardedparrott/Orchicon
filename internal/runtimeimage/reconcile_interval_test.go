package runtimeimage

import (
	"fmt"
	"testing"
	"time"
)

func TestReconcileIntervalStringIsPostgresCompatible(t *testing.T) {
	ttl := 5 * time.Minute
	interval := fmt.Sprintf("%d seconds", int(ttl.Seconds()))
	if interval != "300 seconds" {
		t.Fatalf("unexpected interval %q", interval)
	}
	if ttl.String() == interval {
		t.Fatalf("ttl.String should not be used directly for postgres interval")
	}
}
