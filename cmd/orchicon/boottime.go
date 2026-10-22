package main

// boottime.go — WHEN THIS PROCESS STARTED, which is the reference point that separates a LEFTOVER from a
// process that is starting right now.
//
// The distinction is not pedantry: a check that asks "is anything of this instance running?" while the
// instance is BEING STARTED answers yes about the postgres it has just spawned. See postgres_lock.go's
// bootCutoff for the measured failure that produced (every start refused, blaming its own newborn).
import "time"

var processStartedAt = time.Now()

// Note: `var` rather than a function call at each site so a test can move the reference point without
// sleeping, and so the value is fixed once at process start — which is what "this boot" means.
