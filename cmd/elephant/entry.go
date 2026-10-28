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

// parseRelations converts repeatable TYPE:ENTRY_UUID flag values.
func parseRelations(raw []string) ([]app.RelationInput, error) {
	var out []app.RelationInput
	for _, value := range raw {
		typ, id, ok := strings.Cut(value, ":")
		if !ok || typ == "" || id == "" {
			return nil, fmt.Errorf("%w: relation must be TYPE:ENTRY_UUID", model.ErrInvalidInput)
		}
		out = append(out, app.RelationInput{Type: typ, EntryID: id})
	}
	return out, nil
}

// parseCreate reads the flags shared by add and supersede.
func parseCreate(name string, args []string, logs io.Writer) (app.CreateInput, error) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(logs)
	title := f.String("title", "", "concise title")
	body := f.String("body", "", "context and reasoning")
	target := f.String("target-version", "", "release or milestone")
	supersedes := f.String("supersedes", "", "active decision ID this decision replaces")
	var files, relations fileFlags
	f.Var(&files, "file", "related repository-relative file; repeatable")
	f.Var(&relations, "relation", "TYPE:ENTRY_UUID; repeatable")
	if err := parseFlags(f, args); err != nil {
		return app.CreateInput{}, err
	}
	if f.NArg() != 0 {
		return app.CreateInput{}, model.ErrInvalidInput
	}
	links, err := parseRelations(relations)
	if err != nil {
		return app.CreateInput{}, err
	}
	in := app.CreateInput{Title: *title, Body: *body, RelatedFiles: files, Relations: links, Supersedes: *supersedes}
	if *target != "" {
		in.TargetVersion = target
	}
	return in, nil
}

func runAdd(ctx context.Context, s *app.Service, cwd string, args []string, logs io.Writer) (any, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("%w: add requires fact, decision, or task", model.ErrInvalidInput)
	}
	in, err := parseCreate("add", args[1:], logs)
	if err != nil {
		return nil, err
	}
	return s.Add(ctx, cwd, model.Kind(args[0]), in)
}

// runSupersede mirrors supersede_decision: ID is the active decision to close.
func runSupersede(ctx context.Context, s *app.Service, cwd string, args []string, logs io.Writer) (any, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return nil, fmt.Errorf("%w: supersede requires DECISION_ID", model.ErrInvalidInput)
	}
	in, err := parseCreate("supersede", args[1:], logs)
	if err != nil {
		return nil, err
	}
	if in.Supersedes != "" {
		return nil, fmt.Errorf("%w: supersede takes the replaced decision as its argument", model.ErrInvalidInput)
	}
	in.Supersedes = args[0]
	return s.Add(ctx, cwd, model.Decision, in)
}

// runUpdate mirrors update_fact, update_decision, and update_task. Only
// flags that are given change the entry; --file replaces all file links and
// --clear-files removes them.
func runUpdate(ctx context.Context, s *app.Service, cwd string, args []string, logs io.Writer) (any, error) {
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		return nil, fmt.Errorf("%w: update requires fact|decision|task ID", model.ErrInvalidInput)
	}
	kind, id := model.Kind(args[0]), args[1]
	f := flag.NewFlagSet("update", flag.ContinueOnError)
	f.SetOutput(logs)
	title := f.String("title", "", "new title")
	body := f.String("body", "", "new body")
	status := f.String("status", "", "new lifecycle status")
	target := f.String("target-version", "", "release or milestone; empty clears it")
	clearFiles := f.Bool("clear-files", false, "remove all related file links")
	var files, relations fileFlags
	f.Var(&files, "file", "replacement related file; repeatable")
	f.Var(&relations, "relation", "TYPE:ENTRY_UUID to add; repeatable")
	if err := parseFlags(f, args[2:]); err != nil {
		return nil, err
	}
	if f.NArg() != 0 {
		return nil, model.ErrInvalidInput
	}
	if *clearFiles && len(files) > 0 {
		return nil, fmt.Errorf("%w: use either --file or --clear-files", model.ErrInvalidInput)
	}
	links, err := parseRelations(relations)
	if err != nil {
		return nil, err
	}
	in := app.UpdateInput{Relations: links}
	f.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "title":
			in.Title = title
		case "body":
			in.Body = body
		case "status":
			in.Status = status
		case "target-version":
			in.TargetVersion = target
		}
	})
	if len(files) > 0 || *clearFiles {
		replacement := []string(files)
		if replacement == nil {
			replacement = []string{}
		}
		in.RelatedFiles = &replacement
	}
	return s.Update(ctx, cwd, kind, id, in)
}

// closeCommands mirror complete_task, cancel_task, and retire_fact.
var closeCommands = map[string]struct {
	kind   model.Kind
	status string
}{
	"complete": {model.Task, "done"},
	"cancel":   {model.Task, "cancelled"},
	"retire":   {model.Fact, "retired"},
}

func runClose(ctx context.Context, s *app.Service, cwd, command string, args []string) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("%w: %s requires one entry ID", model.ErrInvalidInput, command)
	}
	op := closeCommands[command]
	return s.Update(ctx, cwd, op.kind, args[0], app.UpdateInput{Status: &op.status})
}
