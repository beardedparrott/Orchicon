package scheduler

import "testing"

// TestResolveImageFromValues covers the one-shot image-agreement rule:
// empty → base (""), one distinct image wins, duplicates are fine, and
// two different images conflict (a single container can't serve both).
func TestResolveImageFromValues(t *testing.T) {
	cases := []struct {
		name    string
		values  []string
		want    string
		wantErr bool
	}{
		{"empty -> base", []string{}, "", false},
		{"all unset -> base", []string{"", ""}, "", false},
		{"single image", []string{"pyside6-gui:latest"}, "pyside6-gui:latest", false},
		{"duplicates agree", []string{"gui:1", "gui:1", "gui:1"}, "gui:1", false},
		{"unset + one set", []string{"", "gui:1", ""}, "gui:1", false},
		{"conflict", []string{"gui:1", "ml:2"}, "", true},
		{"conflict among many", []string{"gui:1", "gui:1", "ml:2", "gui:1"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveImageFromValues(c.values)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}
