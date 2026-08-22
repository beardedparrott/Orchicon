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

// TestValidateSessionTTLs exercises the session TTL validation constants
// and boundary conditions enforced by validateSessionTTLs.
func TestValidateSessionTTLs(t *testing.T) {
	cases := []struct {
		name      string
		accessTTL int64
		refreshTTL int64
		wantErr   bool
	}{
		{"both zero — leave unchanged", 0, 0, false},
		{"access zero, refresh set", 0, 86400, false},
		{"access set, refresh zero", 900, 0, false},
		{"both set — valid", 900, 86400, false},
		{"access below minimum (29)", 29, 0, true},
		{"access at minimum (30)", 30, 0, false},
		{"access above maximum (86401)", 86401, 0, true},
		{"access at maximum (86400)", 86400, 0, false},
		{"refresh below minimum (299)", 900, 299, true},
		{"refresh at minimum (300)", 900, 300, false},
		{"refresh above maximum (31536001)", 900, 31536001, true},
		{"refresh at maximum (31536000)", 900, 31536000, false},
		{"refresh equal to access — rejected", 900, 900, true},
		{"refresh less than access — rejected", 900, 800, true},
		{"refresh just above access — accepted", 900, 901, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSessionTTLs(c.accessTTL, c.refreshTTL)
			gotErr := err != nil
			if gotErr != c.wantErr {
				t.Errorf("validateSessionTTLs(%d, %d) error = %v, wantErr = %v",
					c.accessTTL, c.refreshTTL, err, c.wantErr)
			}
		})
	}
}
