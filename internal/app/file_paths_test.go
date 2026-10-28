package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"elephant/internal/app"
	"elephant/internal/model"
)

// File arguments are relative to the working directory, like Git pathspecs,
// and are stored relative to the repository root, so the same file has one
// key wherever the command ran.
func TestFileArgumentsResolveFromWorkingDirectory(t *testing.T) {
	ctx := context.Background()
	root := repo(t)
	sub := filepath.Join(root, "internal", "app")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	writeWorkingFile(t, root, filepath.Join("internal", "app", "service.go"), "package app\n")
	commitFile(t, root, "internal/app/service.go", "add service")
	s := evidenceService(t, "paths.zova")

	fact, err := s.Add(ctx, sub, model.Fact, app.CreateInput{Title: "From subdirectory", Body: "B", RelatedFiles: []string{"service.go", "../model/model.go"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ cwd, file string }{
		{root, "internal/app/service.go"},
		{sub, "service.go"},
		{sub, "./service.go"},
		{root, "internal/model/model.go"},
	} {
		history, err := s.History(ctx, c.cwd, c.file, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(history) != 1 || history[0].ID != fact.Entry.ID {
			t.Fatalf("history(%s, %s) = %+v, want the fact", c.cwd, c.file, history)
		}
	}
	results, err := s.Search(ctx, sub, app.SearchFilters{File: "service.go"})
	if err != nil || len(results) != 1 {
		t.Fatalf("search by file from subdirectory = %+v, %v", results, err)
	}
	view, err := s.AddEvidence(ctx, sub, fact.Entry.ID, "service.go", 0)
	if err != nil || view.Path != "internal/app/service.go" {
		t.Fatalf("evidence from subdirectory = %+v, %v", view, err)
	}
	checkpoint, err := s.AddCheckpoint(ctx, sub, app.CheckpointInput{Summary: "S", RelatedFiles: []string{"service.go"}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range checkpoint.Relations {
		found = found || strings.HasSuffix(r.To, ":internal/app/service.go")
	}
	if !found {
		t.Fatalf("checkpoint relations = %+v, want internal/app/service.go", checkpoint.Relations)
	}

	for _, c := range []struct{ cwd, file string }{
		{root, "../outside.go"},
		{sub, "../../../outside.go"},
		{sub, "/etc/passwd"},
		{sub, ""},
		{sub, "."},
	} {
		_, err := s.Add(ctx, c.cwd, model.Fact, app.CreateInput{Title: "T", Body: "B", RelatedFiles: []string{c.file}})
		if !errors.Is(err, model.ErrInvalidInput) {
			t.Errorf("file %q from %s = %v, want invalid input", c.file, c.cwd, err)
		}
	}
}
