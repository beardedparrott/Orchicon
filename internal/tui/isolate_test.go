package tui

// isolate_test.go — TEST HERMETICITY, set up once for the whole package.
//
// WHY THIS FILE EXISTS. NewApp reads the fold preferences from ~/.orchicon/config at CONSTRUCTION, so a
// shell built by a test inherited the DEVELOPER's own state. The failure was machine-dependent and
// therefore invisible in CI on a clean box:
//
//	--- FAIL: TestSpaceOnAFolderMarksItsMembers
//	    grouping_test.go:433: space on a folder must mark its two members, got 0: []
//
// The fixture uses category id "cat-1". An operator who had collapsed a folder had
// "conversations:cat-1" in their config, so the fixture's folder started COLLAPSED, its members were not
// rows, and `space` had nothing to mark. The same category id was all it took.
//
// THE FIX IS AT THE READER (see collapsedPrefsPath) and this is the switch it consults. Setting it in an
// `init` means EVERY test in this package is isolated without any of them mentioning it — and a test
// written later cannot forget, which is the property that matters: an opt-out that individual tests must
// remember is one that the next test will not.
//
// `init` rather than TestMain because there is nothing to tear down: the flag is set for the whole test
// binary and never cleared, and no test wants the real config to be reachable by accident.
//
// Tests that WANT to exercise the persistence set ORCHICON_CONFIG_DIR to a temp dir, which
// collapsedPrefsPath honours even here — see prefs_wire_test.go.
func init() { collapsePrefsDisabled = true }
