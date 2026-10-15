package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"elephant/internal/app"
	"elephant/internal/model"
)

func TestInspectShowsIncomingRelationsAndEvidence(t *testing.T) {
	ctx := context.Background()
	cwd := evidenceRepo(t)
	s := evidenceService(t, "inspect.zova")
	decision, err := s.Add(ctx, cwd, model.Decision, app.CreateInput{Title: "Choice", Body: "Reason"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.Add(ctx, cwd, model.Task, app.CreateInput{Title: "Build", Body: "Do it", Relations: []app.RelationInput{{Type: "implements", EntryID: decision.Entry.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	fact, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Runtime", Body: "Name is runtime", Relations: []app.RelationInput{{Type: "supports", EntryID: decision.Entry.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddEvidence(ctx, cwd, fact.Entry.ID, "runtime.go", 3); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := s.AddCheckpoint(ctx, cwd, app.CheckpointInput{Summary: "Session", Relations: []app.RelationInput{{Type: "relates", EntryID: decision.Entry.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := s.Add(ctx, cwd, model.Decision, app.CreateInput{Title: "Better choice", Body: "Better reason", Supersedes: decision.Entry.ID})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, cwd, decision.Entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Relations) != 0 {
		t.Fatalf("outgoing relations = %+v, want none", got.Relations)
	}
	want := map[string]string{
		"entry:" + task.Entry.ID:        "implements",
		"entry:" + fact.Entry.ID:        "supports",
		"entry:" + replacement.Entry.ID: "supersedes",
	}
	var checkpointEdge bool
	for _, r := range got.Incoming {
		if r.To != "entry:"+decision.Entry.ID {
			t.Fatalf("incoming edge %+v does not point at the inspected entry", r)
		}
		if strings.HasSuffix(r.From, ":"+checkpoint.Checkpoint.ID) && r.Type == "relates" {
			checkpointEdge = true
			continue
		}
		if want[r.From] != r.Type {
			t.Fatalf("unexpected incoming edge %+v", r)
		}
		delete(want, r.From)
	}
	if len(want) != 0 || !checkpointEdge {
		t.Fatalf("missing incoming edges %v (checkpoint edge %v) in %+v", want, checkpointEdge, got.Incoming)
	}
	if len(got.Evidence) != 0 {
		t.Fatalf("decision evidence = %+v, want none", got.Evidence)
	}

	gotFact, err := s.Get(ctx, cwd, fact.Entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotFact.Evidence) != 1 || gotFact.Evidence[0].Path != "runtime.go" || gotFact.Evidence[0].Line != 3 {
		t.Fatalf("fact evidence = %+v", gotFact.Evidence)
	}
	if len(gotFact.Relations) != 1 || gotFact.Relations[0].Type != "supports" {
		t.Fatalf("fact outgoing relations = %+v", gotFact.Relations)
	}
	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"incoming":[]`, `"evidence":[]`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("record JSON %s lacks %s", encoded, field)
		}
	}
}

func TestRecallListsMemoryForChangedFiles(t *testing.T) {
	ctx := context.Background()
	cwd := evidenceRepo(t)
	writeWorkingFile(t, cwd, "other.go", "package other\n")
	commitFile(t, cwd, "other.go", "add other")
	s := evidenceService(t, "changed.zova")

	open, err := s.Add(ctx, cwd, model.Task, app.CreateInput{Title: "Edit runtime", Body: "Open work", RelatedFiles: []string{"runtime.go"}})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := s.Add(ctx, cwd, model.Task, app.CreateInput{Title: "Old runtime work", Body: "Done", RelatedFiles: []string{"runtime.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Update(ctx, cwd, model.Task, finished.Entry.ID, app.UpdateInput{Status: ptr("done")}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Other", Body: "Unchanged file", RelatedFiles: []string{"other.go"}}); err != nil {
		t.Fatal(err)
	}

	clean, err := s.Recall(ctx, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if clean.ChangedFiles == nil || len(clean.ChangedFiles) != 0 {
		t.Fatalf("clean checkout changed files = %#v, want empty", clean.ChangedFiles)
	}

	writeWorkingFile(t, cwd, "runtime.go", committedRuntimeGo+"// edited\n")
	writeWorkingFile(t, cwd, "untracked.go", "package untracked\n")
	got, err := s.Recall(ctx, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ChangedFiles) != 1 || got.ChangedFiles[0].Path != "runtime.go" {
		t.Fatalf("changed files = %+v, want only runtime.go", got.ChangedFiles)
	}
	ids := map[string]bool{}
	for _, e := range got.ChangedFiles[0].Entries {
		ids[e.ID] = true
	}
	if len(ids) != 2 || !ids[open.Entry.ID] || !ids[finished.Entry.ID] {
		t.Fatalf("runtime.go entries = %+v, want the open and the finished task", got.ChangedFiles[0].Entries)
	}
}
