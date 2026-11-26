package opencode

// container_resources.go — WHAT THE CONTAINER LOOKED LIKE WHEN WE RECYCLED IT.
//
// A session-store failure surfaces as "opencode_session_error: Failed to execute statement": opencode's
// wrapper for a SQLite write that failed, WITHOUT the SQLite error. The repair path recycles the run's
// container, which discards the container's /tmp (where the store lives) and with it the only evidence of
// why the write failed. Observed twice on one run: the container's 2 GiB /tmp tmpfs at 100% full (Go test
// binaries), and the container at the cap of its 4 GiB memory limit with /tmp only 35% full. Each recycle
// gave the same worker a fresh container to fill the same way, ~8 minutes at a time.
//
// So before the kill, sample the container and say what it saw. Best effort and bounded: a wedged container
// must never delay the recycle it is being sampled for.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/runtime"
)

// resourceProbeCommand prints one `key=value` line per fact. Every read tolerates a missing file (cgroup v1,
// a path that is not there) by printing an empty value instead of failing the whole probe.
const resourceProbeCommand = `
echo "mem=$(cat /sys/fs/cgroup/memory.current 2>/dev/null) $(cat /sys/fs/cgroup/memory.max 2>/dev/null)"
echo "swap=$(cat /sys/fs/cgroup/memory.swap.current 2>/dev/null) $(cat /sys/fs/cgroup/memory.swap.max 2>/dev/null)"
echo "events=$(tr '\n' ' ' </sys/fs/cgroup/memory.events 2>/dev/null)"
echo "tmp=$(df -kP /tmp 2>/dev/null | tail -1 | awk '{print $2, $3, $4}')"
echo "biggest=$(du -sk /tmp/* 2>/dev/null | sort -rn | head -4 | tr '\t\n' ': ')"
echo "store=$(ls -l /tmp/orchicon-serve-data/opencode 2>/dev/null | awk '/opencode.db/{print $5":"$9}' | tr '\n' ' ')"
echo "storelog=$(grep -ihE 'locked|busy|statement|sqlite|no space' /tmp/orchicon-serve-data/opencode/log/*.log 2>/dev/null | tail -2 | cut -c1-240 | tr '\n' '|')"
`

// containerResources is the parsed probe.
type containerResources struct {
	MemCurrent, MemMax int64 // bytes; MemMax 0 means "max" (no cap) or unknown
	TmpSizeK, TmpUsedK int64
	TmpAvailK          int64
	TmpKnown           bool
	Events             string
	Biggest            string
	Store              string
	StoreLog           string

	// Exhausted is true when the container was at a limit that the worker, not the store, caused; Reason
	// says which. A recycle does not cure either: the next container gets the same budget.
	Exhausted bool
	Reason    string
}

// scratchFullFraction / memoryAtCapFraction are how close to a limit counts as "at" it.
const (
	scratchFullFraction = 0.97
	memoryAtCapFraction = 0.95
)

// parseResourceProbe reads resourceProbeCommand's output. Garbage in yields a zero value with Exhausted
// false, never a panic and never a false alarm.
func parseResourceProbe(out string) containerResources {
	var r containerResources
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "mem":
			f := strings.Fields(v)
			if len(f) >= 1 {
				r.MemCurrent, _ = strconv.ParseInt(f[0], 10, 64)
			}
			if len(f) >= 2 {
				r.MemMax, _ = strconv.ParseInt(f[1], 10, 64) // "max" fails to parse → 0 → no cap
			}
		case "tmp":
			f := strings.Fields(v)
			if len(f) == 3 {
				size, e1 := strconv.ParseInt(f[0], 10, 64)
				used, e2 := strconv.ParseInt(f[1], 10, 64)
				avail, e3 := strconv.ParseInt(f[2], 10, 64)
				if e1 == nil && e2 == nil && e3 == nil && size > 0 {
					r.TmpSizeK, r.TmpUsedK, r.TmpAvailK, r.TmpKnown = size, used, avail, true
				}
			}
		case "events":
			r.Events = v
		case "biggest":
			r.Biggest = v
		case "store":
			r.Store = v
		case "storelog":
			r.StoreLog = v
		}
	}

	var reasons []string
	if r.TmpKnown && float64(r.TmpUsedK) >= scratchFullFraction*float64(r.TmpSizeK) {
		reasons = append(reasons, "/tmp is full ("+strconv.FormatInt(r.TmpUsedK/1024, 10)+" of "+
			strconv.FormatInt(r.TmpSizeK/1024, 10)+" MiB) — the session store lives there and cannot write")
	}
	if r.MemMax > 0 && float64(r.MemCurrent) >= memoryAtCapFraction*float64(r.MemMax) {
		reasons = append(reasons, "memory is at its cap ("+strconv.FormatInt(r.MemCurrent>>20, 10)+" of "+
			strconv.FormatInt(r.MemMax>>20, 10)+" MiB; tmpfs pages count against it)")
	}
	if len(reasons) > 0 {
		r.Exhausted = true
		r.Reason = strings.Join(reasons, "; ")
	}
	return r
}

// logContainerResources samples the run's container and logs what it found. It never returns an error and
// never waits long: it exists to leave evidence, not to gate the recycle.
func (a *Adapter) logContainerResources(runtimeWorkflowID string) {
	if a.rt == nil || runtimeWorkflowID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := a.rt.Exec(ctx, runtimeWorkflowID, runtime.ExecRequest{Command: resourceProbeCommand})
	if err != nil || res == nil {
		a.log.Warn("runtime container resources at recycle: probe failed — the container is probably wedged",
			"run", runtimeWorkflowID, "error", err)
		return
	}
	r := parseResourceProbe(res.Stdout)
	attrs := []any{
		"run", runtimeWorkflowID,
		"mem_bytes", r.MemCurrent, "mem_cap_bytes", r.MemMax, "memory_events", r.Events,
		"tmp_used_kib", r.TmpUsedK, "tmp_size_kib", r.TmpSizeK, "tmp_biggest", r.Biggest,
		"store", r.Store, "store_log", r.StoreLog,
	}
	if r.Exhausted {
		a.log.Warn("runtime container ran out of scratch or memory — recycling will NOT cure this: the next "+
			"container has the same budget and the same worker will fill it the same way. Raise "+
			"ORCHICON_RUNTIME_MEMORY / ORCHICON_RUNTIME_TMPFS, or move the worker's scratch (Go's TMPDIR) "+
			"off tmpfs", append(attrs, "reason", r.Reason)...)
		return
	}
	a.log.Warn("runtime container resources at recycle: no limit reached — the store failure has another cause "+
		"(see store_log)", attrs...)
}
