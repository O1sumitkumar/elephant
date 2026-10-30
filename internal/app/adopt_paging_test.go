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
	"elephant/internal/storage"
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

	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	sender := id()
	start := time.Now().UTC().Add(-time.Hour)
	head := status.Git.Head
	err = db.Transact(ctx, func(tx storage.Tx) error {
		local := status.Project
		for i := 0; i < localCount; i++ {
			title, body := "shared title", "shared body"
			at := start
			if i > 0 {
				title = fmt.Sprintf("local %d", i)
				body = "local"
				at = start.Add(time.Duration(i) * time.Millisecond)
			}
			e := model.Entry{
				ID: id(), ActorID: "tester", Kind: model.Fact, Title: title, Body: body, Status: "active",
				StartCommit: &head, CreatedAt: at, UpdatedAt: at,
			}
			if err := e.Validate(); err != nil {
				return err
			}
			if err := tx.Put(local, e); err != nil {
				return err
			}
		}
		src, err := tx.EnsureSource(model.Project{Identity: "github.com/test/shared", Name: "shared"}, sender)
		if err != nil {
			return err
		}
		for i := 0; i < remoteCount; i++ {
			title, body := fmt.Sprintf("remote %d", i), "remote"
			if i == 0 {
				title, body = "shared title", "shared body"
			}
			at := start.Add(time.Duration(i) * time.Millisecond)
			e := model.Entry{
				ID: id(), Kind: model.Fact, Title: title, Body: body, Status: "active", ActorID: "peer",
				CreatedAt: at, UpdatedAt: at,
			}
			if err := e.Validate(); err != nil {
				return err
			}
			if err := tx.Put(src, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
