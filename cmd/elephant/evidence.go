package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"elephant/internal/app"
	"elephant/internal/model"
)

// runEvidence exposes attach, inspect, verify, refresh, and remove for a fact's
// recorded source evidence. Only refresh and remove write, and neither changes
// the fact's lifecycle state.
func runEvidence(ctx context.Context, service *app.Service, cwd string, args []string, logs io.Writer) (any, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("%w: evidence add|list|verify|refresh|remove", model.ErrInvalidInput)
	}
	op, args := args[0], args[1:]
	if len(args) == 0 {
		return nil, fmt.Errorf("%w: evidence %s requires an entry or evidence ID", model.ErrInvalidInput, op)
	}
	id, rest := args[0], args[1:]
	fs := flag.NewFlagSet("evidence "+op, flag.ContinueOnError)
	fs.SetOutput(logs)
	switch op {
	case "add":
		path := fs.String("file", "", "repository-relative file that established the fact")
		line := fs.Int("line", 0, "optional one-based line number")
		if err := parseFlags(fs, rest); err != nil {
			return nil, err
		}
		if fs.NArg() != 0 || strings.TrimSpace(*path) == "" {
			return nil, fmt.Errorf("%w: evidence add requires --file PATH", model.ErrInvalidInput)
		}
		return service.AddEvidence(ctx, cwd, id, *path, *line)
	case "list", "verify":
		if len(rest) != 0 {
			return nil, model.ErrInvalidInput
		}
		if op == "verify" {
			return service.VerifyEvidence(ctx, cwd, id)
		}
		return service.ListEvidence(ctx, cwd, id)
	case "refresh":
		evidenceID := fs.String("evidence", "", "refresh only this evidence row")
		if err := parseFlags(fs, rest); err != nil {
			return nil, err
		}
		if fs.NArg() != 0 {
			return nil, model.ErrInvalidInput
		}
		return service.RefreshEvidence(ctx, cwd, id, *evidenceID)
	case "remove":
		if len(rest) != 0 {
			return nil, model.ErrInvalidInput
		}
		return service.RemoveEvidence(ctx, cwd, id)
	}
	return nil, fmt.Errorf("%w: unknown evidence subcommand %q", model.ErrInvalidInput, op)
}
