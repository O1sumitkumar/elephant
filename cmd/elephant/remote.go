package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/remote"
	"elephant/internal/remote/ash"
)

func runRemote(ctx context.Context, s *app.Service, cwd string, args []string, logs io.Writer) (any, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("%w: remote list|add|remove|ensure-project|send|recall|inbox|diff", model.ErrInvalidInput)
	}
	op := args[0]
	args = args[1:]
	if op == "list" {
		if len(args) != 0 {
			return nil, model.ErrInvalidInput
		}
		return s.ListRemotes(ctx)
	}
	if op == "inbox" {
		if len(args) != 0 {
			return nil, model.ErrInvalidInput
		}
		return s.Inbox(ctx)
	}
	if op == "diff" {
		if len(args) != 1 {
			return nil, fmt.Errorf("%w: remote diff requires a remote name or source UUID", model.ErrInvalidInput)
		}
		return s.Diff(ctx, cwd, args[0])
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("%w: remote name required", model.ErrInvalidInput)
	}
	name := args[0]
	args = args[1:]
	f := flag.NewFlagSet("remote "+op, flag.ContinueOnError)
	f.SetOutput(logs)
	switch op {
	case "remove":
		if len(args) != 0 {
			return nil, model.ErrInvalidInput
		}
		return map[string]string{"removed": name}, s.RemoveRemote(ctx, name)
	case "add":
		backend := f.String("backend", "ash", "transport backend")
		host := f.String("host", "", "ASH host alias")
		config := f.String("ash-config", "", "ASH configuration file")
		binary := f.String("elephant-path", "", "remote Elephant executable")
		if err := parseFlags(f, args); err != nil {
			return nil, err
		}
		if f.NArg() != 0 {
			return nil, model.ErrInvalidInput
		}
		raw, _ := json.Marshal(ash.Config{Host: *host, ConfigPath: *config, ElephantPath: *binary})
		return s.AddRemote(ctx, name, *backend, string(raw))
	case "ensure-project", "recall":
		if len(args) != 0 {
			return nil, model.ErrInvalidInput
		}
		operation := "project.ensure"
		if op == "recall" {
			operation = "project.recall"
		}
		m, err := s.BuildMessage(ctx, cwd, operation, "", app.CreateInput{})
		if err != nil {
			return nil, err
		}
		return s.Send(ctx, remote.ASH{}, name, m)
	case "send":
		if len(args) == 0 {
			return nil, model.ErrInvalidInput
		}
		kind := model.Kind(args[0])
		args = args[1:]
		title := f.String("title", "", "concise title")
		body := f.String("body", "", "context")
		version := f.String("target-version", "", "release or milestone")
		supersedes := f.String("supersedes", "", "remote decision ID")
		var files, relations, evidence fileFlags
		f.Var(&files, "file", "repository-relative file; repeatable")
		f.Var(&relations, "relation", "TYPE:ENTRY_UUID; repeatable")
		f.Var(&evidence, "evidence", "local evidence row ID to carry; repeatable")
		if err := parseFlags(f, args); err != nil {
			return nil, err
		}
		if f.NArg() != 0 {
			return nil, model.ErrInvalidInput
		}
		in := app.CreateInput{Title: *title, Body: *body, RelatedFiles: files, Supersedes: *supersedes}
		if *version != "" {
			in.TargetVersion = version
		}
		for _, raw := range relations {
			var typ, id string
			for i, c := range raw {
				if c == ':' {
					typ, id = raw[:i], raw[i+1:]
					break
				}
			}
			if typ == "" || id == "" {
				return nil, model.ErrInvalidInput
			}
			in.Relations = append(in.Relations, app.RelationInput{Type: typ, EntryID: id})
		}
		for _, raw := range evidence {
			in.Evidence = append(in.Evidence, app.EvidenceInput{EntryID: raw})
		}
		m, err := s.BuildMessage(ctx, cwd, "entry.send", kind, in)
		if err != nil {
			return nil, err
		}
		return s.Send(ctx, remote.ASH{}, name, m)
	}
	return nil, model.ErrInvalidInput
}
