package zova

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	native "github.com/ata-sesli/zova/bindings/go"

	"elephant/internal/model"
)

// Zova never migrates a storage format on open, so Elephant upgrades an older
// .zova file before opening it. Migration is copy-forward: Zova writes the
// current format to a new file, the original bytes are kept as a backup, and
// the new file atomically replaces the database path. A lock file serializes
// processes that open the same database at once.
const (
	migrationLockWait  = 60 * time.Second
	migrationLockStale = 5 * time.Minute
)

func migrateFormat(path string) error {
	if !needsFormatMigration(path) {
		return nil
	}
	unlock, err := lockMigration(path)
	if err != nil {
		return err
	}
	defer unlock()
	// Another process may have migrated while this one waited for the lock.
	info, err := native.ProbeFormat(path)
	if err != nil || info.Compatibility != native.FormatMigratable {
		return nil
	}
	return migrateLocked(path, info.FormatVersion)
}

func needsFormatMigration(path string) bool {
	info, err := native.ProbeFormat(path)
	// Probe failures fall through to Open, which reports them.
	return err == nil && info.Compatibility == native.FormatMigratable
}

func lockMigration(path string) (func(), error) {
	lock := path + ".migrate.lock"
	deadline := time.Now().Add(migrationLockWait)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(lock) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: create migration lock: %w", model.ErrMigration, err)
		}
		if stat, statErr := os.Stat(lock); statErr == nil && time.Since(stat.ModTime()) > migrationLockStale {
			os.Remove(lock)
			continue
		}
		// Waiters must not open the database while the lock holder migrates:
		// on Windows an open handle makes replacing the file fail. The holder
		// re-probes after acquiring the lock instead.
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: another process is migrating %s; remove %s if no Elephant process is running", model.ErrMigration, path, lock)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func migrateLocked(path string, format uint32) error {
	base := strings.TrimSuffix(path, ".zova")
	if busy := sidecars(path); len(busy) > 0 {
		return fmt.Errorf("%w: cannot migrate %s while another process has it open (%s); stop other Elephant processes and retry", model.ErrMigration, path, strings.Join(busy, ", "))
	}
	staging := fmt.Sprintf("%s.format-%d-migrating.zova", base, format)
	removeWithSidecars(staging)
	if err := native.MigrateDatabase(path, staging); err != nil {
		removeWithSidecars(staging)
		return fmt.Errorf("%w: migrate Zova format %d: %w", model.ErrMigration, format, err)
	}
	// The migration opened the source; a sidecar now means a concurrent writer.
	if busy := append(sidecars(path), sidecars(staging)...); len(busy) > 0 {
		removeWithSidecars(staging)
		return fmt.Errorf("%w: cannot replace %s while another process has it open (%s); stop other Elephant processes and retry", model.ErrMigration, path, strings.Join(busy, ", "))
	}
	backup := fmt.Sprintf("%s.format-%d-backup-%s.zova", base, format, time.Now().UTC().Format("20060102T150405Z"))
	if err := copyFile(path, backup); err != nil {
		removeWithSidecars(staging)
		return fmt.Errorf("%w: back up %s before migration: %w", model.ErrMigration, path, err)
	}
	if err := os.Chmod(staging, 0600); err != nil {
		removeWithSidecars(staging)
		return err
	}
	if err := os.Rename(staging, path); err != nil {
		removeWithSidecars(staging)
		return fmt.Errorf("%w: replace %s with the migrated database: %w", model.ErrMigration, path, err)
	}
	return nil
}

// sidecars lists SQLite journal files next to a database. They exist only
// while a connection is writing or after a crash, and must never be paired
// with a different database file.
func sidecars(path string) []string {
	var found []string
	for _, suffix := range []string{"-journal", "-wal"} {
		if _, err := os.Stat(path + suffix); err == nil {
			found = append(found, filepath.Base(path+suffix))
		}
	}
	return found
}

func removeWithSidecars(path string) {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}
