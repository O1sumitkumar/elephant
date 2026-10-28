package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage/zova"
)

// Adoption follows local lifecycle rules: an adopted supersedes link marks
// the replaced local decision superseded, a finished entry stays finished,
// and a decision is superseded locally only through a supersedes edge.
func TestAdoptKeepsLifecycleRules(t *testing.T) {
	ctx := context.Background()
	cwd := sharedRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "adoptstatus.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	id := func() string { return uuid.Must(uuid.NewV7()).String() }
	sender := id()
	now := time.Now().UTC()
	receive := func(kind model.Kind, status string, relations []app.RelationInput) string {
		t.Helper()
		entryID := id()
		_, err := s.Receive(ctx, app.Message{ProtocolVersion: 1, MessageID: id(), SenderElephantID: sender,
			Project:   app.ProjectRef{Identity: "github.com/test/shared", Name: "shared"},
			Operation: "entry.send",
			Entry:     &model.Entry{ID: entryID, Kind: kind, Title: "T " + status, Body: "B", Status: status, ActorID: "peer", CreatedAt: now, UpdatedAt: now},
			Relations: relations})
		if err != nil {
			t.Fatal(err)
		}
		return entryID
	}
	adopt := func(entryID string) app.AdoptResult {
		t.Helper()
		out, err := s.Adopt(ctx, cwd, entryID, sender)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	get := func(localID string) model.Entry {
		t.Helper()
		r, err := s.Get(ctx, cwd, localID)
		if err != nil {
			t.Fatal(err)
		}
		return r.Entry
	}

	done := adopt(receive(model.Task, "done", nil))
	if done.Entry.Status != "done" || done.Entry.EndCommit == nil {
		t.Fatalf("adopted done task = %+v, want done with an end commit", done.Entry)
	}
	retired := adopt(receive(model.Fact, "retired", nil))
	if retired.Entry.Status != "retired" {
		t.Fatalf("adopted retired fact = %+v, want retired", retired.Entry)
	}
	// Without its replacement there is no supersedes edge to justify the
	// status, so a superseded decision is adopted as active.
	orphan := adopt(receive(model.Decision, "superseded", nil))
	if orphan.Entry.Status != "active" {
		t.Fatalf("adopted superseded decision = %+v, want active", orphan.Entry)
	}

	oldRemote := receive(model.Decision, "active", nil)
	old := adopt(oldRemote)
	replacement := adopt(receive(model.Decision, "active", []app.RelationInput{{Type: "supersedes", EntryID: oldRemote}}))
	if replacement.Entry.Status != "active" {
		t.Fatalf("replacement = %+v, want active", replacement.Entry)
	}
	if got := get(old.Entry.ID); got.Status != "superseded" || got.EndCommit == nil {
		t.Fatalf("replaced decision = %+v, want superseded with an end commit", got)
	}

	// The replaced decision is no longer active, so a second replacement's
	// supersedes link is skipped, as a local supersede would refuse it.
	second := adopt(receive(model.Decision, "active", []app.RelationInput{{Type: "supersedes", EntryID: oldRemote}}))
	if len(second.Skipped) != 1 || !strings.Contains(second.Skipped[0], "supersedes") {
		t.Fatalf("second replacement skipped = %+v, want the supersedes link", second.Skipped)
	}
	for _, r := range second.Relations {
		if r.Type == "supersedes" {
			t.Fatalf("second replacement linked %+v", r)
		}
	}

	findings, err := s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Code == app.CodeSupersessionAnomaly && f.Source == "local" {
			t.Fatalf("adoption left a supersession anomaly: %+v", f)
		}
	}
}
