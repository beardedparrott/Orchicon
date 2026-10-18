package main

// plane.go — `orchicon plane <status|start|stop|restart> [instance]`.
//
// THIS IS THE REFERENCE CALLER of the shared local plane-control layer (internal/planecontrol), and the entry
// point the shell launchers invoke — `scripts/container.sh` delegates its plane_start/plane_stop/plane_status
// here whenever a built binary exists, so the shell stops being a second implementation of the lifecycle.
//
// IT CONTAINS NO LIFECYCLE LOGIC: argv parsing and printing only. A second copy of "how do we stop a plane,
// and what do we do when we cannot identify it" is exactly what the shared layer exists to prevent. Richer
// CLI UX (flags, JSON output, richer help) belongs to the CLI-verb work item; this is the mechanism's front
// door, and it is deliberately the same mechanism the TUI and the desktop app call.

import (
	"fmt"
	"os"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

func runPlane(args []string) error {
	instance := env("ORCHICON_INSTANCE", "dev")
	if len(args) == 0 {
		return fmt.Errorf("plane needs a verb: status | start | stop | restart (instance is optional, default %q)", instance)
	}
	verb := args[0]
	if len(args) > 1 {
		instance = args[1]
	}
	// The instance name forms a DIRECTORY NAME, so a name that could climb out of the instance state dir is
	// refused here rather than turning into a path — ONE guard, shared with `restart`/`start`/`stop`, so the
	// rule cannot be relaxed in one verb and forgotten in another.
	if err := validInstanceName(instance); err != nil {
		return err
	}

	opts := planecontrol.Defaults(instance, os.Stdout)
	switch verb {
	case "status":
		st, err := planecontrol.StatusOf(opts)
		if err != nil {
			return err
		}
		fmt.Println(st.String())
		return nil
	case "start":
		return planecontrol.Start(opts)
	case "stop":
		return planecontrol.Stop(opts)
	case "restart":
		return planecontrol.Restart(opts)
	default:
		return fmt.Errorf("unknown plane verb %q — use status | start | stop | restart", verb)
	}
}
