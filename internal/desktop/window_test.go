package desktop

import "testing"

// AC7: the title carries the app name AND the instance it points at.
func TestTitle(t *testing.T) {
	info := BuildInfo("http://127.0.0.1:8091", "v1", "v1")
	if got, want := Title(info, "127.0.0.1:8091"), "Orchicon — prod (127.0.0.1:8091)"; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}
	// An unrecognised port says so rather than guessing "dev".
	unknown := BuildInfo("http://example.test:9999", "v1", "v1")
	if got, want := Title(unknown, "example.test:9999"), "Orchicon — unknown (example.test:9999)"; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}
}

func TestWindowOptionsFor(t *testing.T) {
	info := BuildInfo("http://127.0.0.1:8092", "v1", "v1")
	opts := WindowOptionsFor(info, "127.0.0.1:8092", Geometry{W: 1600, H: 1000})
	if opts.Title != "Orchicon — test (127.0.0.1:8092)" {
		t.Errorf("Title = %q", opts.Title)
	}
	if opts.W != 1600 || opts.H != 1000 {
		t.Errorf("remembered size not used: %dx%d", opts.W, opts.H)
	}
	if opts.MinW != MinSize().W || opts.MinH != MinSize().H {
		t.Errorf("minimum = %dx%d, want %v", opts.MinW, opts.MinH, MinSize())
	}
}
