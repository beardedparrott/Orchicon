package screenkit

// systemzone.go — WHICH ZONE IS THE OPERATOR IN? (best effort, and honest when it cannot tell)
//
// A recurring schedule stores a WALL CLOCK plus the ZONE it is expressed in, because "09:00 every day" is
// not an instant: it is 09:00 CDT in July and 09:00 CST in January, an hour apart. Something has to stamp
// that zone, and the only party that knows the operator's zone is the operator's own machine. So the
// client resolves it here and the server stores it.
//
// THE TRAP THIS EXISTS TO AVOID. The obvious implementation is:
//
//	time.Local.String()
//
// which returns "America/Chicago" when TZ names a zone — and returns the literal string "Local" when the
// zone comes from /etc/localtime, which is the NORMAL configuration on most Linux desktops. "Local" then
// passes a naive check, because time.LoadLocation("Local") RETURNS NO ERROR: it resolves to whatever
// machine is reading it. A schedule stamped "Local" by this client would therefore be stored fine and
// then interpreted on the SERVER as the SERVER's local zone — UTC in a container — so the operator's
// 09:00 becomes 03:00 while every validation step reported success. That is why this function never
// returns "Local", and why the server refuses it by name too.
//
// SO: a real IANA name, or nothing. "Nothing" is a legitimate answer and is deliberately not papered
// over with a guess — see SystemZoneName's note on why an unresolvable zone is reported rather than
// silently replaced with UTC.

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// zoneinfoDir is the conventional location of the IANA database, used to recover a zone NAME from the
// /etc/localtime symlink.
const zoneinfoDir = "/usr/share/zoneinfo/"

// SystemZoneName returns the operator's zone as an IANA name ("America/Chicago"), or "" when it cannot
// be determined.
//
// IT RETURNS "" RATHER THAN GUESSING. The tempting fallback is UTC, and it is wrong: it would silently
// schedule a Central-time operator's job five or six hours off while the UI claimed everything was fine
// — precisely the class of bug this whole change exists to remove. An empty answer lets the caller SAY
// so ("system timezone could not be determined"), which the operator can act on. The server treats "" as
// the legacy UTC semantic, so nothing breaks; it just does not pretend.
//
// Resolution order, most authoritative first:
//
//  1. time.Local's name, when it is a REAL zone name. TZ set by the operator or the desktop is the
//     strongest signal, and it is what Go itself is using to render their times.
//  2. /etc/localtime resolved through its symlink. This is the case that would otherwise yield "Local",
//     and it is recoverable: the symlink points INTO the zoneinfo tree, so the zone name is the path
//     suffix. A copied file (not a symlink) defeats it, and that is fine — it falls through to "".
//
// A name is only accepted when it is unambiguous. Bare abbreviations like "CST" are NOT accepted even
// though LoadLocation resolves them, because in the IANA database those are legacy FIXED-OFFSET entries
// that ignore daylight saving: a "CST" schedule would be an hour wrong all summer. A real zone either is
// UTC/GMT or is qualified by a region ("Area/Location"), so that is the test.
func SystemZoneName() string {
	// 1. Go's own answer, when it is a name it can stand behind.
	if name := strings.TrimSpace(time.Local.String()); name != "" && name != "Local" {
		if isUnambiguousZone(name) {
			return name
		}
	}
	// 2. Recover the name from the /etc/localtime symlink.
	if name := zoneFromLocaltimeSymlink(); name != "" {
		return name
	}
	return ""
}

// isUnambiguousZone reports whether name identifies one zone the same way on every machine.
//
// UTC and GMT are unambiguous without a region. Anything else must be region-qualified ("Area/Location"),
// which excludes both the pseudo-zone "Local" and the legacy bare abbreviations whose meaning depends on
// which fixed offset the reader's database happens to carry.
func isUnambiguousZone(name string) bool {
	if name == "UTC" || name == "GMT" {
		return true
	}
	if !strings.Contains(name, "/") {
		return false
	}
	// It must actually resolve — a name with a slash that no database knows is not a zone either.
	_, err := time.LoadLocation(name)
	return err == nil
}

// zoneFromLocaltimeSymlink recovers a zone name from /etc/localtime when it is a symlink into the
// zoneinfo tree, or "" when it is absent, a real file, or pointing somewhere unexpected.
func zoneFromLocaltimeSymlink() string {
	target, err := filepath.EvalSymlinks("/etc/localtime")
	if err != nil {
		return ""
	}
	idx := strings.Index(target, zoneinfoDir)
	if idx < 0 {
		return ""
	}
	name := strings.TrimSuffix(strings.TrimPrefix(target[idx:], zoneinfoDir), "/")
	if name == "" || !isUnambiguousZone(name) {
		return ""
	}
	// Readable check: a listing that cannot be opened is no use to us, and it keeps this honest on a
	// machine where the symlink dangles.
	if _, err := os.Stat(target); err != nil {
		return ""
	}
	return name
}
