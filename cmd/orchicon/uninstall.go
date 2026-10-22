package main

// uninstall.go — taking an instance OFF this machine.
//
// THE OPERATOR: "an uninstall should clean all services/infrastructure. The only thing it technically
// should leave is the data. We may want to put some options in the installer when someone uninstalls, it
// gives them the option to also destroy data and do a nuke options with the wizard."
//
// THERE WAS NOTHING TO DO THAT. The MSI removes its binaries and, because its setup action is conditioned
// on NOT REMOVE, it never stops the services it started:
//
//	<Custom Action="LaunchSetup" After="InstallFinalize" Condition="NOT REMOVE AND SKIPSETUP <> 1" />
//
// So uninstalling left a live services supervisor, a live plane, postgres and nats holding their ports,
// and a data directory — on a machine where the product was "gone". The next install then met an orphan
// holding 5432 (see install.go's pre-clear), which is the reported "uninstall then reinstall does not work".
//
// WHAT IS REMOVED, AND WHAT IS DELIBERATELY KEPT, is the whole design of this file:
//
//	REMOVED by default — everything that is INFRASTRUCTURE, because it is either re-downloadable (the
//	binaries) or regenerated on the next boot (config, the plane's pid/logs, telemetry state):
//	    bin/                  postgres, nats, tempo, loki, victoriametrics, otelcol (re-downloaded)
//	    config/               generated from the embedded templates on every boot
//	    serve/                the plane's pid files and logs
//	    nats/ loki/ tempo/ victoriametrics/   telemetry and event-store state
//	    services.log, ports.json, infra-binaries.json, project-mounts/
//
//	KEPT by default — the operator's DATA, which nothing can regenerate:
//	    postgres/             the cluster: projects, workers, work items, conversations, executions
//	    blobs/                uploaded attachments referenced by that data
//	    secrets/kek           the KEY ENCRYPTION KEY. Deleting it does not delete secrets — it makes
//	                          every tenant secret permanently undecryptable, which is a worse outcome
//	                          than leaving the file.
//	    permission-policy.yaml   an operator-authored file, not ours to remove
//	    backups/              anything the operator asked us to keep
//
//	PURGED with --purge — the whole instance directory, including the cluster. This is the wizard's
//	"also destroy my data" option, and it is the ONLY path that removes the KEK or the cluster. It is
//	irreversible and it says so.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// uninstallInfraDirs are removed on every uninstall: re-downloadable or regenerated on boot.
var uninstallInfraDirs = []string{
	"bin", "config", "serve", "nats", "loki", "tempo", "victoriametrics", "project-mounts",
}

// uninstallInfraFiles are removed on every uninstall, for the same reason.
var uninstallInfraFiles = []string{
	"services.log", "ports.json", "infra-binaries.json",
}

// uninstallDataDirs are the operator's data. They are removed ONLY with --purge.
var uninstallDataDirs = []string{
	"postgres", "blobs", "secrets", "backups",
}

// runUninstall implements `orchicon uninstall [instance] [--purge]`.
//
// IT STOPS BEFORE IT DELETES, always, and refuses to delete anything while a process of this instance is
// still alive — removing a running postgres's data directory is corruption, not uninstallation.
func runUninstall(args []string, out io.Writer) error {
	purge := false
	var rest []string
	for _, a := range args {
		switch a {
		case "--purge", "-purge", "--destroy-data", "--nuke":
			purge = true
		default:
			rest = append(rest, a)
		}
	}
	instance, err := parseInstanceTarget(rest, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		return err
	}

	dataDir := hostDataDirFor(instance)
	if !instanceExists(instance) {
		fmt.Fprintf(out, "nothing is installed for instance %q (no data directory at %s)\\n", instance, dataDir)
		return nil
	}

	// 1. STOP EVERYTHING THIS INSTANCE HAS RUNNING. Not optional, and not best-effort: the deletion below
	//    must not race a live postgres or supervisor, and an uninstall that leaves services running is the
	//    reported defect this whole file exists to fix.
	ports, _ := servicesPortsFor(instance)
	fmt.Fprintf(out, "stopping instance %q\\n", instance)
	if err := stopInstanceCompletely(instance, ports, out); err != nil {
		return fmt.Errorf("could not stop instance %q, so nothing was removed: %w\\n"+
			"  Uninstalling a RUNNING instance would delete a live database's files. Stop it first:\\n"+
			"    %s stop %s", instance, err, filepath.Base(os.Args[0]), instance)
	}

	// 2. REMOVE THE INFRASTRUCTURE.
	for _, d := range uninstallInfraDirs {
		p := filepath.Join(dataDir, d)
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	for _, f := range uninstallInfraFiles {
		p := filepath.Join(dataDir, f)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	fmt.Fprintf(out, "removed the services, their binaries and this instance's generated config\\n")

	// 3. THE DATA: kept, or destroyed on request.
	if purge {
		// THE WHOLE DIRECTORY, so nothing is quietly left behind that a later install would inherit.
		// This is irreversible, and it is the only path that removes the cluster or the KEK.
		if err := os.RemoveAll(dataDir); err != nil {
			return fmt.Errorf("purge %s: %w", dataDir, err)
		}
		fmt.Fprintf(out, "DESTROYED all data for instance %q, including the database cluster and the\\n"+
			"secrets key (%s)\\n", instance, dataDir)
		return nil
	}
	kept := append([]string{}, uninstallDataDirs...)
	sort.Strings(kept)
	fmt.Fprintf(out, "kept your data in %s\\n", dataDir)
	for _, d := range kept {
		if _, err := os.Stat(filepath.Join(dataDir, d)); err == nil {
			fmt.Fprintf(out, "  %s/\\n", d)
		}
	}
	fmt.Fprintf(out, "  the database cluster is intact — reinstalling this instance reuses it\\n")
	fmt.Fprintf(out, "  to destroy it too (IRREVERSIBLE):  %s uninstall %s --purge\\n",
		filepath.Base(os.Args[0]), instance)
	return nil
}
