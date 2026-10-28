package app_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage/zova"
)

func TestEvidenceRemoteTransferPreservesLocations(t *testing.T) {
	ctx := context.Background()
	cwd := evidenceRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "sender.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sender := app.New(db)
	entryID, want := attachFact(t, sender, cwd, "runtime.go", 3)
	report, err := sender.ListEvidence(ctx, cwd, entryID)
	if err != nil || len(report) != 1 {
		t.Fatal(report, err)
	}
	id := func() string { return uuid.Must(uuid.NewV7()).String() }
	m := app.Message{ProtocolVersion: 1, MessageID: id(), SenderElephantID: id(), Project: app.ProjectRef{Identity: "github.com/test/repo", Name: "repo"}, Operation: "entry.send",
		Entry:    &model.Entry{ID: id(), Kind: model.Fact, Title: "remote fact", Body: "context", Status: "active", ActorID: "actor", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		Evidence: []model.Evidence{{ID: id(), EntryID: "", Path: "src/main.go", Commit: want.Commit, Blob: want.Blob, CreatedAt: time.Now().UTC(), VerifiedAt: time.Now().UTC()}}}
	m.Evidence[0].EntryID = m.Entry.ID
	if _, err = sender.Receive(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := sender.RecallSource(ctx, m.Project, m.SenderElephantID, "")
	if err != nil || len(got.Evidence) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	view := got.Evidence[0]
	if view.Path != "src/main.go" || view.Commit != want.Commit || view.Blob != want.Blob || view.EntryID != m.Entry.ID {
		t.Fatalf("%+v", view)
	}
	if view.State != model.EvidenceUnavailable {
		t.Fatalf("remote project must report unavailable, got %+v", view)
	}
	// Re-sending an identical payload is idempotent; changing evidence is a conflict.
	m.MessageID = id()
	if _, err = sender.Receive(ctx, m); err != nil {
		t.Fatal(err)
	}
	m.MessageID = id()
	m.Evidence[0].Blob = "changed"
	if _, err = sender.Receive(ctx, m); err == nil {
		t.Fatal("changed evidence accepted as a retry")
	}
	m.MessageID = id()
	m.Evidence[0].Blob = want.Blob
	m.Evidence = append(m.Evidence, model.Evidence{ID: id(), EntryID: m.Entry.ID, Path: "src/other.go", CreatedAt: time.Now().UTC(), VerifiedAt: time.Now().UTC()})
	if _, err = sender.Receive(ctx, m); err == nil {
		t.Fatal("duplicate evidence ID accepted")
	}
}

func TestRemoteSendCarriesLocalEvidence(t *testing.T) {
	ctx := context.Background()
	cwd := evidenceRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "carry.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sender := app.New(db)
	other := repo(t)
	for _, args := range [][]string{
		{"-C", other, "remote", "add", "origin", "https://github.com/test/carry.git"},
		{"-C", cwd, "remote", "add", "origin", "https://github.com/test/carry.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	m, err := sender.BuildMessage(ctx, other, "entry.send", model.Fact, app.CreateInput{Title: "Carried", Body: "Context"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Evidence) != 0 {
		t.Fatalf("empty evidence input must not travel: %+v", m.Evidence)
	}
	entryID, want := attachFact(t, sender, cwd, "runtime.go", 1)
	m, err = sender.BuildMessage(ctx, cwd, "entry.send", model.Fact, app.CreateInput{Title: "Carried", Body: "Context", Evidence: []app.EvidenceInput{{EntryID: want.ID}}})
	if err != nil {
		t.Fatalf("carrying local evidence: %v", err)
	}
	if len(m.Evidence) != 1 || m.Evidence[0].EntryID != m.Entry.ID || m.Evidence[0].Path != "runtime.go" {
		t.Fatalf("%+v", m.Evidence)
	}
	if m.Evidence[0].ID == want.ID {
		t.Fatalf("carried evidence must be rebound to a fresh ID: %+v", m.Evidence)
	}
	if m.Evidence[0].Commit != want.Commit || m.Evidence[0].Blob != want.Blob {
		t.Fatalf("carried evidence lost its provenance: %+v", m.Evidence)
	}
	_ = entryID
	sentry := uuid.Must(uuid.NewV7()).String()
	if _, err = sender.Receive(ctx, app.Message{ProtocolVersion: 1, MessageID: uuid.Must(uuid.NewV7()).String(), SenderElephantID: sentry, Project: app.ProjectRef{Identity: "github.com/test/carry", Name: "carry"}, Operation: "entry.send", Entry: &model.Entry{ID: m.Entry.ID, Kind: model.Fact, Title: "Sent", Body: "Carried body", Status: "active", ActorID: "actor", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, Evidence: []model.Evidence{{ID: m.Evidence[0].ID, EntryID: m.Entry.ID, Path: "src/hijack.go", CreatedAt: time.Now().UTC(), VerifiedAt: time.Now().UTC()}}}); err != nil {
		t.Fatalf("carried evidence rejected: %v", err)
	}
	got, err := sender.RecallSource(ctx, app.ProjectRef{Identity: "github.com/test/carry", Name: "carry"}, sentry, "")
	if err != nil || len(got.Evidence) != 1 || got.Evidence[0].Path != "src/hijack.go" || got.Evidence[0].State != model.EvidenceUnavailable {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = sender.BuildMessage(ctx, cwd, "entry.send", model.Fact, app.CreateInput{Title: "Carried", Body: "Context", Evidence: []app.EvidenceInput{{EntryID: "missing"}}}); err == nil {
		t.Fatal("unknown evidence row accepted")
	}
	// Evidence is facts-only locally, so decisions and tasks never carry it,
	// whether this side sends them or a peer does.
	for _, k := range []model.Kind{model.Decision, model.Task} {
		_, err = sender.BuildMessage(ctx, cwd, "entry.send", k, app.CreateInput{Title: "Carried", Body: "Context", Evidence: []app.EvidenceInput{{EntryID: want.ID}}})
		if !errors.Is(err, model.ErrInvalidInput) {
			t.Fatalf("sending a %s with evidence = %v, want invalid input", k, err)
		}
		entry := &model.Entry{ID: uuid.Must(uuid.NewV7()).String(), Kind: k, Title: "Sent", Body: "B", Status: model.DefaultStatus(k), ActorID: "actor", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		_, err = sender.Receive(ctx, app.Message{ProtocolVersion: 1, MessageID: uuid.Must(uuid.NewV7()).String(), SenderElephantID: sentry, Project: app.ProjectRef{Identity: "github.com/test/carry", Name: "carry"}, Operation: "entry.send", Entry: entry, Evidence: []model.Evidence{{ID: uuid.Must(uuid.NewV7()).String(), EntryID: entry.ID, Path: "runtime.go", CreatedAt: time.Now().UTC(), VerifiedAt: time.Now().UTC()}}})
		if !errors.Is(err, model.ErrInvalidInput) {
			t.Fatalf("receiving a %s with evidence = %v, want invalid input", k, err)
		}
	}
}
