package app_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage/zova"
)

// TestInboxDiffReadEveryEntry covers projects with more than one 200-entry
// page on both sides (issue #27). The oldest remote entry and the oldest local
// entry share their content, so they sort past the first page on each side
// and are only matched when Diff reads every page.
func TestInboxDiffReadEveryEntry(t *testing.T) {
	const remoteCount, localCount = 205, 203
	ctx := context.Background()
	cwd := sharedRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "paging.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	id := func() string { return uuid.Must(uuid.NewV7()).String() }

	if _, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "shared title", Body: "shared body"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < localCount; i++ {
		if _, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: fmt.Sprintf("local %d", i), Body: "local"}); err != nil {
			t.Fatal(err)
		}
	}

	sender := id()
	start := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < remoteCount; i++ {
		title, body := fmt.Sprintf("remote %d", i), "remote"
		if i == 0 {
			title, body = "shared title", "shared body"
		}
		at := start.Add(time.Duration(i) * time.Millisecond)
		if _, err := s.Receive(ctx, app.Message{ProtocolVersion: 1, MessageID: id(), SenderElephantID: sender,
			Project:   app.ProjectRef{Identity: "github.com/test/shared", Name: "shared"},
			Operation: "entry.send",
			Entry:     &model.Entry{ID: id(), Kind: model.Fact, Title: title, Body: body, Status: "active", ActorID: "peer", CreatedAt: at, UpdatedAt: at}}); err != nil {
			t.Fatal(err)
		}
	}

	inbox, err := s.Inbox(ctx)
	if err != nil || len(inbox) != 1 {
		t.Fatalf("%d groups, %v", len(inbox), err)
	}
	if got := len(inbox[0].Entries); got != remoteCount {
		t.Fatalf("inbox has %d entries, want %d", got, remoteCount)
	}

	diff, err := s.Diff(ctx, cwd, sender)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Remote) != remoteCount || len(diff.Local) != localCount {
		t.Fatalf("diff read %d remote and %d local entries, want %d and %d", len(diff.Remote), len(diff.Local), remoteCount, localCount)
	}
	if len(diff.Conflicting) != 1 || diff.Conflicting[0].Title != "shared title" {
		t.Fatalf("conflicting = %+v, want the shared entry", diff.Conflicting)
	}
	if len(diff.RemoteOnly) != remoteCount-1 {
		t.Fatalf("remote only has %d entries, want %d", len(diff.RemoteOnly), remoteCount-1)
	}
}
