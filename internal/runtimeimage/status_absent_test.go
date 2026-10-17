package runtimeimage

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestImageStatusProtoAbsent pins the wire mapping of the seeder's ABSENT
// state. Without it the row would surface in the UI as UNSPECIFIED, and the
// Runtime Images page could not offer the fetch action for a not-downloaded
// image (it must be distinct from draft, failed and unspecified).
func TestImageStatusProtoAbsent(t *testing.T) {
	if got := imageStatusProto(RuntimeImageStatusAbsent); got != apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_ABSENT {
		t.Fatalf("imageStatusProto(absent) = %v, want RUNTIME_IMAGE_STATUS_ABSENT", got)
	}
	if imageStatusProto(RuntimeImageStatusAbsent) == apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_UNSPECIFIED {
		t.Error("absent must not map to UNSPECIFIED — the UI distinguishes not-downloaded from unknown")
	}
	// The pre-existing states must keep their mapping.
	for status, want := range map[string]apiv1.RuntimeImageStatus{
		"draft":    apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_DRAFT,
		"building": apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_BUILDING,
		"ready":    apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY,
		"failed":   apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_FAILED,
	} {
		if got := imageStatusProto(status); got != want {
			t.Errorf("imageStatusProto(%q) = %v, want %v", status, got, want)
		}
	}
	// Every distinct lifecycle state must map to a distinct wire value.
	seen := map[apiv1.RuntimeImageStatus]string{}
	for _, s := range []string{"draft", "building", "ready", "failed", RuntimeImageStatusAbsent} {
		v := imageStatusProto(s)
		if prev, ok := seen[v]; ok {
			t.Errorf("%q and %q collapse to the same wire status %v", s, prev, v)
		}
		seen[v] = s
	}
}
