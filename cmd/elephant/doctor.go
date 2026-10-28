package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"elephant/internal/agent"
	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage/zova"
	transport "elephant/internal/transport/mcp"
)

const (
	statusOK      = "ok"
	statusWarning = "warning"
	statusError   = "error"
)

type diagnosticCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type diagnosticReport struct {
	Status string            `json:"status"`
	Checks []diagnosticCheck `json:"checks"`
}

func detail(err error) string { return transport.PublicError(err).Error() }

// diagnose checks installation, storage, Git, MCP registration, actor, and
// remote configuration. It never returns an error so doctor can report every
// finding at once.
func diagnose(ctx context.Context, dbPath string) diagnosticReport {
	checks := []diagnosticCheck{}
	add := func(name, status, text string) {
		checks = append(checks, diagnosticCheck{Name: name, Status: status, Detail: text})
	}
	executable, err := os.Executable()
	if err != nil {
		executable = ""
		add("executable", statusError, "cannot locate the running executable: "+detail(err))
	} else {
		add("executable", statusOK, fmt.Sprintf("%s (elephant %s)", executable, transport.Version))
	}
	var store *zova.Store
	if _, statErr := os.Stat(dbPath); errors.Is(statErr, os.ErrNotExist) {
		add("database", statusWarning, dbPath+" does not exist yet; Elephant creates it on first use")
	} else if statErr != nil {
		add("database", statusError, "cannot read "+dbPath+": "+detail(statErr))
	} else if store, err = zova.Open(dbPath); err != nil {
		if errors.Is(err, model.ErrSchema) {
			add("database", statusError, dbPath+" is not a compatible Elephant database; explicit migration is required")
		} else {
			add("database", statusError, "cannot open "+dbPath+": "+detail(err))
		}
		store = nil
	} else {
		add("database", statusOK, dbPath+" is readable and schema-compatible")
		defer store.Close()
	}
	if path, lookErr := exec.LookPath("git"); lookErr != nil {
		add("git", statusError, "git was not found on PATH; Elephant requires Git at runtime")
	} else if version, versionErr := exec.CommandContext(ctx, path, "--version").Output(); versionErr != nil {
		add("git", statusError, "git at "+path+" failed: "+detail(versionErr))
	} else {
		add("git", statusOK, path+" ("+strings.TrimSpace(string(version))+")")
	}
	if executable == "" {
		add("mcp", statusWarning, "cannot report the MCP launch command without the executable path")
	} else {
		checks = append(checks, mcpCheck(executable, dbPath))
	}
	if actor := strings.TrimSpace(os.Getenv("ELEPHANT_ACTOR_ID")); actor == "" || actor == "unknown" {
		add("actor", statusWarning, "ELEPHANT_ACTOR_ID is not set; new entries are recorded as actor_id \"unknown\"")
	} else {
		add("actor", statusOK, actor)
	}
	if store == nil {
		add("remotes", statusWarning, "skipped because the database is unavailable")
	} else if remotes, remoteErr := app.New(store).ListRemotes(ctx); remoteErr != nil {
		add("remotes", statusError, "cannot read configured remotes: "+detail(remoteErr))
	} else if len(remotes) == 0 {
		add("remotes", statusWarning, "no ASH remotes are configured; remote sync stays disabled")
	} else {
		names := make([]string, 0, len(remotes))
		for _, remote := range remotes {
			names = append(names, remote.Name+" ("+remote.Backend+")")
		}
		add("remotes", statusOK, fmt.Sprintf("%d configured: %s", len(remotes), strings.Join(names, ", ")))
	}
	report := diagnosticReport{Status: statusOK, Checks: checks}
	for _, check := range checks {
		switch {
		case check.Status == statusError:
			report.Status = statusError
		case check.Status == statusWarning && report.Status == statusOK:
			report.Status = statusWarning
		}
	}
	return report
}

func mcpCheck(executable, database string) diagnosticCheck {
	command := strings.Join(agent.Command(executable, database), " ")
	var configured, problems []string
	for _, name := range agent.Names() {
		state, err := agent.Inspect(name, nil)
		switch {
		case err != nil:
			problems = append(problems, name+": "+detail(err))
		case state.Configured:
			configured = append(configured, name+" ("+state.ConfigPath+")")
		}
	}
	if len(problems) > 0 {
		return diagnosticCheck{Name: "mcp", Status: statusError, Detail: strings.Join(problems, "; ")}
	}
	if len(configured) == 0 {
		return diagnosticCheck{Name: "mcp", Status: statusWarning, Detail: "no coding agent has Elephant configured; run elephant setup codex. Launch command: " + command}
	}
	return diagnosticCheck{Name: "mcp", Status: statusOK, Detail: "configured for " + strings.Join(configured, ", ") + "; launch command: " + command}
}

// runDoctor reports installation health. Unusable configurations exit nonzero
// so scripts and agents can detect them.
func runDoctor(ctx context.Context, out, logs io.Writer, args []string, dbPath string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(logs)
	structured := fs.Bool("json", false, "print machine-readable output")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: doctor takes no arguments", model.ErrInvalidInput)
	}
	report := diagnose(ctx, dbPath)
	if *structured {
		if err := writeJSON(out, report); err != nil {
			return err
		}
	} else {
		for _, check := range report.Checks {
			fmt.Fprintf(out, "%-7s %-11s %s\n", strings.ToUpper(check.Status), check.Name, check.Detail)
		}
		if report.Status == statusWarning {
			fmt.Fprintln(out, "doctor reported warnings; review the WARNING entries above")
		}
	}
	failures := 0
	for _, check := range report.Checks {
		if check.Status == statusError {
			failures++
		}
	}
	if failures > 0 {
		return fmt.Errorf("%w: %d of %d checks failed", model.ErrDiagnostics, failures, len(report.Checks))
	}
	return nil
}
