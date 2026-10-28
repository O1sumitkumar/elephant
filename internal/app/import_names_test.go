package app_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage"
	"elephant/internal/storage/zova"
)

func otherExportRepo(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", p},
		{"-C", p, "remote", "add", "origin", "https://github.com/test/other.git"},
		{"-C", p, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "--allow-empty", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	return p
}

// Import receipts used to be keyed by archive name alone, so a second
// project imported under the default name failed as conflicting content.
func TestImportDifferentProjectsUnderSameArchiveName(t *testing.T) {
	ctx := context.Background()
	cwdA, cwdB := exportRepo(t), otherExportRepo(t)
	s := populatedService(t, ctx, cwdA, filepath.Join(t.TempDir(), "names.zova"))
	if _, err := s.Add(ctx, cwdB, model.Fact, app.CreateInput{Title: "Other", Body: "B"}); err != nil {
		t.Fatal(err)
	}
	rawA, rawB := exportBytes(t, s, cwdA), exportBytes(t, s, cwdB)
	srcA, err := s.Import(ctx, rawA, "")
	if err != nil {
		t.Fatal(err)
	}
	srcB, err := s.Import(ctx, rawB, "")
	if err != nil {
		t.Fatalf("second project under the default archive name: %v", err)
	}
	if srcA.ID == srcB.ID || srcA.Identity == srcB.Identity {
		t.Fatalf("projects share a source table: %+v %+v", srcA, srcB)
	}
	again, err := s.Import(ctx, rawB, "")
	if err != nil || again.ID != srcB.ID {
		t.Fatalf("identical reimport = %+v, %v; want existing source %s", again, err, srcB.ID)
	}
}

// An archive imported before receipts carried the project keeps only the
// name-keyed receipt; conflicting content must still fail against it.
func TestImportChecksLegacyReceipt(t *testing.T) {
	ctx := context.Background()
	cwd := exportRepo(t)
	dbFile := filepath.Join(t.TempDir(), "legacy.zova")
	s := populatedService(t, ctx, cwd, dbFile)
	raw := exportBytes(t, s, cwd)
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	db, err := zova.Open(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Transact(ctx, func(tx storage.Tx) error {
		if _, err := tx.EnsureSource(model.Project{Identity: status.Project.Identity, Name: status.Project.Name}, "import:legacy"); err != nil {
			return err
		}
		_, err := tx.Message("import:legacy", "digest-of-older-content", "import")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(ctx, raw, "legacy"); err == nil || !strings.Contains(err.Error(), "already imported with different content") {
		t.Fatalf("import against a conflicting legacy receipt = %v, want a content conflict", err)
	}
}
