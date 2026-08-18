package settings

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// TestSettingsProtoMaxConcurrentRunsRoundTrip verifies the optional
// max_concurrent_runs field (proto) survives the row conversion with its
// "explicitly set vs. absent" distinction intact: a nil proto pointer must
// NOT clobber the persisted value (MaxConcurrentRunsSet=false), while a
// present pointer — including 0, which means "clear the cap" — must.
func TestSettingsProtoMaxConcurrentRunsRoundTrip(t *testing.T) {
	cases := []struct {
		name       string
		protoValue *int32
		wantVal    int
		wantSet    bool
	}{
		{"absent", nil, 0, false},
		{"explicit zero clears cap", int32Ptr(0), 0, true},
		{"positive cap", int32Ptr(4), 4, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := settingsProtoToRow(&apiv1.TenantSettings{MaxConcurrentRuns: c.protoValue})
			if row.MaxConcurrentRuns != c.wantVal {
				t.Errorf("MaxConcurrentRuns = %d, want %d", row.MaxConcurrentRuns, c.wantVal)
			}
			if row.MaxConcurrentRunsSet != c.wantSet {
				t.Errorf("MaxConcurrentRunsSet = %v, want %v", row.MaxConcurrentRunsSet, c.wantSet)
			}
		})
	}
}

// TestSettingsRowToProtoMaxConcurrentRuns verifies the row→proto direction
// always fills the pointer with the persisted value (0 means "no cap").
func TestSettingsRowToProtoMaxConcurrentRuns(t *testing.T) {
	proto := settingsRowToProto(&db.TenantSettingsRow{MaxConcurrentRuns: 3})
	if proto.MaxConcurrentRuns == nil {
		t.Fatal("MaxConcurrentRuns nil, want non-nil pointer")
	}
	if *proto.MaxConcurrentRuns != 3 {
		t.Fatalf("MaxConcurrentRuns = %d, want 3", *proto.MaxConcurrentRuns)
	}
	zero := settingsRowToProto(&db.TenantSettingsRow{MaxConcurrentRuns: 0})
	if zero.MaxConcurrentRuns == nil || *zero.MaxConcurrentRuns != 0 {
		t.Fatalf("MaxConcurrentRuns for 0 = %v, want pointer to 0", zero.MaxConcurrentRuns)
	}
}

func int32Ptr(v int32) *int32 { return &v }
