package main

// boottime.go — WHEN THIS PROCESS STARTED, which is the reference point that separates a LEFTOVER
// from a process that is starting right now.
import "time"

var processStartedAt = time.Now()

// Note: `var` rather than a function call at each site so a test can move the reference point without
// sleeping, and so the value is fixed once at process start — which is what "this boot" means.
