package kit2

import tea "github.com/charmbracelet/bubbletea"

// Focus is the ONE documented rule for key ownership in kit2:
//
//	Exactly one region owns the keyboard at any time. A region receives keys
//	only while it is the focused region; every other region is blurred. Tab
//	moves focus forward, Shift+Tab moves it backward, and a left mouse click
//	inside a region focuses that same region. Keys, Tab order, and the mouse
//	therefore all address the SAME region identity — they cannot disagree,
//	because they all route through this one Focus value.
//
// Modality is layered on top, not inside: a Dialog (or a Form) that is open
// takes ownership of the region list (the screen pushes it as the only
// focusable region) so an overlay can never leak keys to the region behind
// it.
type Focus struct {
	Regions []string
	cur     int
}

// NewFocus builds a focus ring in the given region order.
func NewFocus(regions ...string) *Focus {
	return &Focus{Regions: append([]string{}, regions...)}
}

// Current returns the focused region's name ("" when none).
func (f *Focus) Current() string {
	if f.cur < 0 || f.cur >= len(f.Regions) {
		return ""
	}
	return f.Regions[f.cur]
}

// Index returns a region's position (-1 when absent).
func (f *Focus) Index(name string) int {
	for i, r := range f.Regions {
		if r == name {
			return i
		}
	}
	return -1
}

// IsFocused reports whether name owns the keyboard.
func (f *Focus) IsFocused(name string) bool { return f.Current() == name }

// Set focuses the named region. Unknown names are ignored (the ring never
// points at a region that does not exist).
func (f *Focus) Set(name string) bool {
	i := f.Index(name)
	if i < 0 {
		return false
	}
	f.cur = i
	return true
}

// Next / Prev cycle the ring (tab / shift+tab).
func (f *Focus) Next() {
	if len(f.Regions) == 0 {
		return
	}
	f.cur = (f.cur + 1) % len(f.Regions)
}

func (f *Focus) Prev() {
	if len(f.Regions) == 0 {
		return
	}
	f.cur = (f.cur - 1 + len(f.Regions)) % len(f.Regions)
}

// HandleKey consumes the shared focus-movement keys (tab / shift+tab).
// Returns true when it moved focus.
func (f *Focus) HandleKey(k tea.KeyMsg) bool {
	switch k.String() {
	case "tab":
		f.Next()
		return true
	case "shift+tab":
		f.Prev()
		return true
	}
	return false
}

// Click focuses the region a mouse click landed in — the mouse path to the
// SAME region identity the keyboard uses.
func (f *Focus) Click(region string) bool { return f.Set(region) }
