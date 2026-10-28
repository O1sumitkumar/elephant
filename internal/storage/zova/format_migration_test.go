package zova

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	native "github.com/ata-sesli/zova/bindings/go"

	"elephant/internal/model"
	"elephant/internal/storage"
)

// testdata/format11.zova was written by Elephant 0.3.0 (Zova 1.0.0-rc.2,
// storage format 11) for the repository identity github.com/test/format11:
// one decision, one task implementing it with a file link, and one checkpoint.
const format11Fixture = "testdata/format11.zova"

func copyFormat11(t *testing.T) (string, []byte) {
	t.Helper()
	original, err := os.ReadFile(format11Fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "elephant.zova")
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := native.ProbeFormat(path)
	if err != nil || info.Compatibility != native.FormatMigratable || info.FormatVersion != 11 {
		t.Fatalf("fixture probe = %+v %v, want migratable format 11", info, err)
	}
	return path, original
}

// dirNames lists a directory. Migration leaves <db>.lock in place, since a
// lock file is never removed.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestOpenMigratesOlderFormatAndKeepsBackup(t *testing.T) {
	path, original := copyFormat11(t)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open format 11: %v", err)
	}
	err = s.Transact(context.Background(), func(tx storage.Tx) error {
		p, err := tx.Project("github.com/test/format11")
		if err != nil {
			return err
		}
		entries, err := tx.List(p, model.Filter{Limit: 10})
		if err != nil {
			return err
		}
		if len(entries) != 2 {
			t.Errorf("entries = %d, want 2", len(entries))
		}
		for _, e := range entries {
			if e.Kind != model.Task {
				continue
			}
			links, err := tx.Relations(model.EntryNode(p, e.ID))
			if err != nil {
				return err
			}
			if len(links) != 2 {
				t.Errorf("task relations = %+v, want implements and file link", links)
			}
		}
		checkpoints, err := tx.ListCheckpoints(p, 10, 0)
		if err != nil {
			return err
		}
		if len(checkpoints) != 1 || checkpoints[0].Summary != "Format 11 checkpoint" {
			t.Errorf("checkpoints = %+v", checkpoints)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := native.ProbeFormat(path)
	if err != nil || info.Compatibility != native.FormatCurrent {
		t.Fatalf("migrated probe = %+v %v, want current", info, err)
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), "elephant.format-11-backup-*.zova"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %v %v, want one", backups, err)
	}
	kept, err := os.ReadFile(backups[0])
	if err != nil || !bytes.Equal(kept, original) {
		t.Fatalf("backup differs from the original database: %v", err)
	}
	if names := dirNames(t, filepath.Dir(path)); len(names) != 3 {
		t.Fatalf("directory after migration = %v, want only the database, its backup and its lock file", names)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	again.Close()
	if names := dirNames(t, filepath.Dir(path)); len(names) != 3 {
		t.Fatalf("reopen changed the directory: %v", names)
	}
}

func TestConcurrentOpensMigrateOnce(t *testing.T) {
	path, _ := copyFormat11(t)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := Open(path)
			if err == nil {
				err = s.Close()
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("opener %d: %v", i, err)
		}
	}
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "elephant.format-11-backup-*.zova"))
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one migration", backups)
	}
	if names := dirNames(t, filepath.Dir(path)); len(names) != 3 {
		t.Fatalf("directory after concurrent opens = %v", names)
	}
}

func TestMigrationRefusesWhileAnotherWriterHasSidecars(t *testing.T) {
	path, original := copyFormat11(t)
	if err := os.WriteFile(path+"-journal", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open migrated while a journal sidecar existed")
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatalf("refused migration changed the database: %v", err)
	}
	if names := dirNames(t, filepath.Dir(path)); len(names) != 3 {
		t.Fatalf("refused migration left files behind: %v", names)
	}
}

func TestConcurrentOpensWaitForBusyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "elephant.zova")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	for round := 0; round < 5; round++ {
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				s, err := Open(path)
				if err == nil {
					err = s.Close()
				}
				errs[i] = err
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d opener %d: %v", round, i, err)
			}
		}
	}
}

// A migration lock belongs to the process that holds it: however old the lock
// file looks, a waiter takes it only after the holder releases it.
func TestMigrationLockIsNotTakenFromALiveHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "elephant.zova")
	unlock, err := lockDatabase(path, true)
	if err != nil {
		t.Fatal(err)
	}
	locks, _ := filepath.Glob(path + "*.lock")
	old := time.Now().Add(-time.Hour)
	for _, lock := range locks {
		if err := os.Chtimes(lock, old, old); err != nil {
			t.Fatal(err)
		}
	}
	acquired := make(chan func(), 1)
	go func() {
		next, err := lockDatabase(path, true)
		if err != nil {
			t.Error(err)
			next = func() {}
		}
		acquired <- next
	}()
	select {
	case next := <-acquired:
		next()
		unlock()
		t.Fatal("a waiter took the lock while its holder was alive")
	case <-time.After(500 * time.Millisecond):
	}
	unlock()
	select {
	case next := <-acquired:
		next()
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter did not get the lock after it was released")
	}
}

// An open store keeps its database from being replaced. An idle connection
// leaves no journal file, so without a lock a migration would replace the
// file under the store and the store's later writes would be lost.
func TestMigrationWaitsForOpenStores(t *testing.T) {
	wait := migrationLockWait
	migrationLockWait = 300 * time.Millisecond
	t.Cleanup(func() { migrationLockWait = wait })
	path, original := copyFormat11(t)

	// Another process has the older database open.
	other, err := lockDatabase(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); !errors.Is(err, model.ErrMigration) {
		if err == nil {
			s.Close()
		}
		other()
		t.Fatalf("Open while another process has the database open = %v, want a refused migration", err)
	}
	if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, original) {
		t.Fatalf("refused migration changed the database: %v", err)
	}
	other()

	// A store keeps the database open until Close, so a migration waits
	// for it.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open after the other process closed: %v", err)
	}
	if unlock, err := lockDatabase(path, true); !errors.Is(err, model.ErrMigration) {
		if err == nil {
			unlock()
		}
		s.Close()
		t.Fatalf("migration lock while a store is open = %v, want refused", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockDatabase(path, true)
	if err != nil {
		t.Fatalf("migration lock after the store closed: %v", err)
	}
	unlock()
}
