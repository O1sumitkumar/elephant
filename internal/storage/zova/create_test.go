package zova

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"elephant/internal/storage"
)

func storeIdentity(s *Store) (id string, err error) {
	err = s.Transact(context.Background(), func(tx storage.Tx) error {
		id, err = tx.Identity()
		return err
	})
	return
}

// A new database becomes visible only once its schema is committed, so
// processes opening a fresh path at the same moment all get a usable
// database and no half-created file or staging copy is left behind.
func TestConcurrentFirstOpensShareOneDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.zova")
	const openers = 8
	var wg sync.WaitGroup
	errs := make([]error, openers)
	stores := make([]*Store, openers)
	for i := range openers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stores[i], errs[i] = Open(path)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("opener %d: %v", i, err)
		}
		if _, err := storeIdentity(stores[i]); err != nil {
			t.Fatalf("opener %d identity: %v", i, err)
		}
	}
	first, _ := storeIdentity(stores[0])
	for i, s := range stores {
		if id, _ := storeIdentity(s); id != first {
			t.Fatalf("opener %d sees identity %s, want %s: openers got different databases", i, id, first)
		}
		s.Close()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "fresh.zova" && e.Name() != "fresh.zova.lock" {
			t.Errorf("leftover file %s", e.Name())
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '/' && info.Mode().Perm() != 0600 {
		t.Fatalf("database mode = %v, want 0600", info.Mode())
	}
}
