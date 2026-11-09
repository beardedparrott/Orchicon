package main

// plane.go — `orchicon plane <status|start|stop|restart> [instance]`.

import (
	"fmt"
	"os"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

func runPlane(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("plane needs a verb: status | start | stop | restart (instance is optional, default %q)",
			env("ORCHICON_INSTANCE", "dev"))
	}
	verb := args[0]
	// THE INSTANCE COMES FROM THE SHARED PARSER, so `plane start prod` and `plane start --instance
	// prod` both work and an unknown flag is refused rather than read as an instance name.
	instance, err := parseInstanceTarget(args[1:], env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
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
