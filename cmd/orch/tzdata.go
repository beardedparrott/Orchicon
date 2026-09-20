package main

// tzdata.go — the binary carries its own timezone database.
//
// WHY A CLIENT NEEDS IT. `orch` renders every time in the operator's LOCAL zone and now stamps that zone
// onto recurring schedules it creates (screenkit.SystemZoneName). Both need the IANA zone RULES — not
// just an offset — because a zone is a set of rules that change twice a year, and resolving
// "America/Chicago" for a schedule happening in November depends on whether November is CST or CDT.
//
// Those rules normally come from the filesystem (/usr/share/zoneinfo). A STATIC BINARY on a machine
// without them — a slim container, a minimal image, a scratch-based deploy — would find no rules at all,
// and `time.Local` would quietly resolve to UTC. Everything would then render in UTC and schedules would
// be STAMPED UTC, with the UI insisting all the while that it was showing local time. The failure is
// silent in exactly the way this whole piece of work exists to stop.
//
// The blank import EMBEDS the rules in the binary (~450 KB) and registers them as a FALLBACK: the
// filesystem is still preferred, so freshness and the operator's own tzdata are unaffected, and the
// embedded copy is used only when the lookup would otherwise FAIL. That turns a silent UTC fallback into
// a correct answer.
//
// It is an explicit import rather than relying on one arriving transitively (the control plane happens to
// get it through the OPA policy engine). A dependency's side effect is not a guarantee: if OPA were ever
// dropped, the server would silently lose its zone database and nothing would say so.
import _ "time/tzdata"
