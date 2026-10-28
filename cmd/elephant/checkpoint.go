package main

import (
	"context"
	"flag"
	"io"

	"elephant/internal/app"
	"elephant/internal/model"
)

func runCheckpoint(ctx context.Context, s *app.Service, cwd string, args []string, logs io.Writer) (any, error) {
	if len(args) > 0 && args[0] == "checkpoints" {
		args = args[1:]
	}
	f := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
	f.SetOutput(logs)
	summary := f.String("summary", "", "session summary (required)")
	start := f.String("start-commit", "", "starting commit (defaults to previous checkpoint end)")
	var completed, next, commands, failures, files, relations fileFlags
	f.Var(&completed, "completed", "completed work item; repeatable")
	f.Var(&next, "next", "next action; repeatable")
	f.Var(&commands, "commands", "command or check run; repeatable")
	f.Var(&failures, "failures", "unresolved failure; repeatable")
	f.Var(&files, "file", "related repository-relative file; repeatable")
	f.Var(&relations, "relation", "TYPE:ENTRY_UUID; repeatable")
	if err := parseFlags(f, args); err != nil {
		return nil, err
	}
	if f.NArg() != 0 {
		return nil, model.ErrInvalidInput
	}
	in := app.CheckpointInput{Summary: *summary, Completed: completed, Next: next, Commands: commands, Failures: failures, StartCommit: *start, RelatedFiles: files}
	links, err := parseRelations(relations)
	if err != nil {
		return nil, err
	}
	in.Relations = links
	return s.AddCheckpoint(ctx, cwd, in)
}

func runCheckpoints(ctx context.Context, s *app.Service, cwd string, args []string, logs io.Writer) (any, error) {
	f := flag.NewFlagSet("checkpoints", flag.ContinueOnError)
	f.SetOutput(logs)
	limit := f.Int("limit", 10, "maximum checkpoints (1-200)")
	offset := f.Int("offset", 0, "pagination offset")
	if err := parseFlags(f, args); err != nil {
		return nil, err
	}
	if f.NArg() != 0 {
		return nil, model.ErrInvalidInput
	}
	return s.ListCheckpoints(ctx, cwd, *limit, *offset)
}
