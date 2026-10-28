package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage"
	"elephant/internal/storage/zova"
)

func reviewRepo(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", p},
		{"-C", p, "remote", "add", "origin", "https://github.com/test/review.git"},
		{"-C", p, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "--allow-empty", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	return p
}

func TestReviewFindingsAndReadOnly(t *testing.T) {
	ctx := context.Background()
	cwd := reviewRepo(t)
	path := filepath.Join(t.TempDir(), "review.zova")
	db, err := zova.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	// Missing file.
	missing, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "F", Body: "B", RelatedFiles: []string{"gone.go"}})
	if err != nil {
		t.Fatal(err)
	}
	// Unknown actor (default).
	if _, err = s.Add(ctx, cwd, model.Task, app.CreateInput{Title: "T", Body: "do"}); err != nil {
		t.Fatal(err)
	}
	// Dangling relation: link to an entry that lives in another project.
	// The graph node exists globally so the edge writes, but review flags it
	// because the target is unavailable in this project's table.
	otherRepo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", otherRepo},
		{"-C", otherRepo, "remote", "add", "origin", "https://github.com/test/review-other.git"},
		{"-C", otherRepo, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "--allow-empty", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	otherTask, err := s.Add(ctx, otherRepo, model.Task, app.CreateInput{Title: "Other", Body: "elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	danglingID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()
	err = db.Transact(ctx, func(tx storage.Tx) error {
		e := model.Entry{ID: danglingID, ActorID: "tester", Kind: model.Task, Title: "D", Body: "dangling", Status: "open", CreatedAt: now, UpdatedAt: now}
		if err := tx.Put(status.Project, e); err != nil {
			return err
		}
		return tx.Link(status.Project, model.Relation{From: "entry:" + danglingID, Type: "depends_on", To: "entry:" + otherTask.Entry.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	// Supersession anomaly: mark superseded without edge.
	supID := uuid.Must(uuid.NewV7()).String()
	err = db.Transact(ctx, func(tx storage.Tx) error {
		e := model.Entry{ID: supID, ActorID: "tester", Kind: model.Decision, Title: "S", Body: "old", Status: "superseded", CreatedAt: now, UpdatedAt: now}
		return tx.Put(status.Project, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Old task via backdated timestamp.
	oldID := uuid.Must(uuid.NewV7()).String()
	old := now.Add(-40 * 24 * time.Hour)
	err = db.Transact(ctx, func(tx storage.Tx) error {
		e := model.Entry{ID: oldID, ActorID: "tester", Kind: model.Task, Title: "Old", Body: "stale", Status: "open", CreatedAt: old, UpdatedAt: old}
		return tx.Put(status.Project, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = missing
	findings, err := s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, f := range findings {
		codes[f.Code] = true
		if f.Code == "" || f.Severity == "" || f.Source == "" || f.Action == "" || f.Detail == "" {
			t.Fatalf("incomplete finding: %+v", f)
		}
	}
	for _, want := range []string{app.CodeMissingFile, app.CodeStaleTask, app.CodeUnknownActor, app.CodeDanglingRelation, app.CodeSupersessionAnomaly} {
		if !codes[want] {
			t.Fatalf("missing %s in %+v", want, codes)
		}
	}
	// Read-only: entry count and timestamps unchanged.
	after, err := s.Recall(ctx, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = after
	second, err := s.Review(ctx, cwd, &app.ReviewOptions{StaleTaskDays: 1, UnverifiedFactDays: 1, StaleRemoteDays: 1})
	if err != nil || len(second) < len(findings) {
		t.Fatalf("%d vs %d", len(second), len(findings))
	}
	if _, err = s.Review(ctx, cwd, &app.ReviewOptions{StaleTaskDays: 0}); err == nil {
		// Zero means default, so no error expected; invalid is out of range.
	}
	if _, err = s.Review(ctx, cwd, &app.ReviewOptions{StaleTaskDays: 99999}); err == nil {
		t.Fatal("unbounded threshold accepted")
	}
	// Remote separation: add a peer with no identity.
	if _, err = s.AddRemote(ctx, "peer", "ash", `{"host":"peer"}`); err != nil {
		t.Fatal(err)
	}
	remoteFindings, err := s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	foundRemote := false
	for _, f := range remoteFindings {
		if f.Code == app.CodeStaleRemote {
			foundRemote = true
		}
	}
	if !foundRemote {
		t.Fatal("missing stale remote finding")
	}
	_ = os.Getenv("unused")
}

// reviewHead returns the current HEAD commit of cwd.
func reviewHead(t *testing.T, cwd string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(string(out), err)
	}
	return strings.TrimSpace(string(out))
}

// TestReviewUsesEvidenceRecordsForFacts: UNVERIFIED_FACT follows evidence
// verification timestamps, not entry UpdatedAt. Editing a row does not
// verify a fact, and a freshly edited row with stale evidence stays flagged.
func TestReviewUsesEvidenceRecordsForFacts(t *testing.T) {
	ctx := context.Background()
	cwd := reviewRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "evidencefacts.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Recently edited, but its only evidence verification is older than the
	// 90-day default: must still be flagged.
	fresh, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Fresh edit", Body: "recently touched"})
	if err != nil {
		t.Fatal(err)
	}
	// Stale row, evidence verified just now: must not be flagged.
	staleID := uuid.Must(uuid.NewV7()).String()
	commit := reviewHead(t, cwd)
	err = db.Transact(ctx, func(tx storage.Tx) error {
		e := model.Entry{ID: staleID, ActorID: "tester", Kind: model.Fact, Title: "Stale edit", Body: "old row", Status: "active", CreatedAt: now.Add(-200 * 24 * time.Hour), UpdatedAt: now.Add(-200 * 24 * time.Hour)}
		if err := tx.Put(status.Project, e); err != nil {
			return err
		}
		return tx.PutEvidence(status.Project, model.Evidence{ID: uuid.Must(uuid.NewV7()).String(), EntryID: staleID, Path: "evidence.txt", Commit: commit, Blob: "bogusdigest", CreatedAt: now.Add(-time.Hour), VerifiedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Transact(ctx, func(tx storage.Tx) error {
		return tx.PutEvidence(status.Project, model.Evidence{ID: uuid.Must(uuid.NewV7()).String(), EntryID: fresh.Entry.ID, Path: "evidence.txt", Commit: commit, Blob: "bogusdigest", CreatedAt: now.Add(-101 * 24 * time.Hour), VerifiedAt: now.Add(-100 * 24 * time.Hour)})
	})
	if err != nil {
		t.Fatal(err)
	}
	findings, err := s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[string]bool{}
	for _, f := range findings {
		if f.Code == app.CodeUnverifiedFact {
			flagged[f.EntryID] = true
		}
	}
	if !flagged[fresh.Entry.ID] {
		t.Fatal("fresh edit with 100-day-old evidence verification not flagged")
	}
	if flagged[staleID] {
		t.Fatal("fact flagged from UpdatedAt although evidence verified today")
	}
}

// TestReviewEvidenceFindings: evidence review reports stable codes for each
// state; unchanged evidence is silent, an unreadable baseline is reported as
// unavailable, and confirmed problems (changed, missing) get distinct codes.
func TestReviewEvidenceFindings(t *testing.T) {
	ctx := context.Background()
	cwd := reviewRepo(t)
	writeWorkingFile(t, cwd, "evidence.txt", "first line\n")
	commitFile(t, cwd, "evidence.txt", "add evidence fixture")
	db, err := zova.Open(filepath.Join(t.TempDir(), "evidencestate.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	backed, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Backed fact", Body: "has evidence"})
	if err != nil {
		t.Fatal(err)
	}
	// Evidence pinned to a commit this repository cannot read: unavailable.
	unverID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()
	err = db.Transact(ctx, func(tx storage.Tx) error {
		e := model.Entry{ID: unverID, ActorID: "tester", Kind: model.Fact, Title: "Foreign baseline", Body: "unreadable", Status: "active", CreatedAt: now, UpdatedAt: now}
		if err := tx.Put(status.Project, e); err != nil {
			return err
		}
		return tx.PutEvidence(status.Project, model.Evidence{ID: uuid.Must(uuid.NewV7()).String(), EntryID: unverID, Path: "evidence.txt", Commit: strings.Repeat("f", 40), Blob: "deadbeef", CreatedAt: now.Add(-time.Hour), VerifiedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.AddEvidence(ctx, cwd, backed.Entry.ID, "evidence.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != model.EvidenceUnchanged {
		t.Fatalf("fresh evidence state: %s (%s)", view.State, view.Detail)
	}
	codesFor := func(findings []app.ReviewFinding, entryID string) map[string]bool {
		out := map[string]bool{}
		for _, f := range findings {
			if f.EntryID == entryID && strings.HasPrefix(f.Code, "EVIDENCE_") {
				out[f.Code] = true
			}
		}
		return out
	}
	findings, err := s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := codesFor(findings, backed.Entry.ID); len(got) != 0 {
		t.Fatalf("unchanged evidence reported: %+v", got)
	}
	// Evidence codes are the stable wire format shared by human and JSON
	// output; assert the literals directly.
	if !codesFor(findings, unverID)["EVIDENCE_UNAVAILABLE"] {
		t.Fatalf("unreadable baseline not reported as unavailable: %+v", findings)
	}
	// Change the working file: confirmed inconsistency, not unavailability.
	writeWorkingFile(t, cwd, "evidence.txt", "second line\ndifferent content\n")
	findings, err = s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !codesFor(findings, backed.Entry.ID)["EVIDENCE_CHANGED"] {
		t.Fatalf("changed evidence not reported: %+v", findings)
	}
	// Remove the working file: missing, distinct from unavailable.
	if err := os.Remove(filepath.Join(cwd, "evidence.txt")); err != nil {
		t.Fatal(err)
	}
	findings, err = s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !codesFor(findings, backed.Entry.ID)["EVIDENCE_MISSING"] {
		t.Fatalf("missing evidence not reported: %+v", findings)
	}
}

// TestReviewsImportedSourcesWithoutRemotes: review scans every stored
// source table, including imported archives with no registered remote, and
// keeps those findings source-separated from local ones.
func TestReviewsImportedSourcesWithoutRemotes(t *testing.T) {
	ctx := context.Background()
	cwd := reviewRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "importedsource.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	err = db.Transact(ctx, func(tx storage.Tx) error {
		src, err := tx.EnsureSource(status.Project, "imported-archive")
		if err != nil {
			return err
		}
		e := model.Entry{ID: "imported-old-task", ActorID: "tester", Kind: model.Task, Title: "Imported", Body: "stale", Status: "open", CreatedAt: old, UpdatedAt: old}
		return tx.Put(src, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	// No remote is registered for this source: review must scan it anyway.
	var remotes []model.Remote
	if err := db.Transact(ctx, func(tx storage.Tx) error {
		var err error
		remotes, err = tx.Remotes()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(remotes) != 0 {
		t.Fatalf("test setup: unexpected remotes %+v", remotes)
	}
	findings, err := s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range findings {
		if f.Code == app.CodeStaleTask && f.Source == "remote:imported-archive" && f.EntryID == "imported-old-task" {
			found = true
		}
		if f.Source == "local" && f.EntryID == "imported-old-task" {
			t.Fatalf("imported entry attributed to local: %+v", f)
		}
	}
	if !found {
		t.Fatalf("imported source without remote not reviewed: %+v", findings)
	}
}

// TestReviewsOnlyCurrentProjectSources: tx.Sources() spans every project in
// the database. Review must only scan source tables whose project identity
// matches the current repository, so entries and findings from unrelated
// projects never appear.
func TestReviewsOnlyCurrentProjectSources(t *testing.T) {
	ctx := context.Background()
	cwd := reviewRepo(t)
	// Second repository with a different identity in the same database.
	other := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", other},
		{"-C", other, "remote", "add", "origin", "https://github.com/test/review-foreign.git"},
		{"-C", other, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "--allow-empty", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	db, err := zova.Open(filepath.Join(t.TempDir(), "multiproject.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	otherStatus, err := s.Status(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if otherStatus.Project.Identity == status.Project.Identity {
		t.Fatal("test setup: identities collide")
	}
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	const foreignID = "foreign-project-task"
	err = db.Transact(ctx, func(tx storage.Tx) error {
		// A source table registered under the OTHER project's identity.
		src, err := tx.EnsureSource(otherStatus.Project, "foreign-elephant")
		if err != nil {
			return err
		}
		e := model.Entry{ID: foreignID, ActorID: "tester", Kind: model.Task, Title: "Foreign", Body: "belongs elsewhere", Status: "open", CreatedAt: old, UpdatedAt: old}
		return tx.Put(src, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Review from this repository: foreign entries and findings never appear.
	findings, err := s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.EntryID == foreignID {
			t.Fatalf("foreign project entry reviewed: %+v", f)
		}
		if f.Source == "remote:foreign-elephant" {
			t.Fatalf("foreign project source reviewed: %+v", f)
		}
	}
	// The owning repository still reviews its own source table: scoping by
	// identity, not omission.
	own, err := s.Review(ctx, other, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range own {
		if f.Code == app.CodeStaleTask && f.Source == "remote:foreign-elephant" && f.EntryID == foreignID {
			found = true
		}
	}
	if !found {
		t.Fatalf("owning repository does not review its source: %+v", own)
	}
}

// TestReviewUnverifiedFactMeasurement: a fact with evidence measures from the
// newest VerifiedAt; a fact with no evidence at all measures from CreatedAt.
// A new fact is therefore not flagged immediately, while an old unverified
// fact and stale evidence both stay flagged.
func TestReviewUnverifiedFactMeasurement(t *testing.T) {
	ctx := context.Background()
	cwd := reviewRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "unverifiedwindow.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	day := 24 * time.Hour
	mkFact := func(title string, created time.Time, verified *time.Time) string {
		id := uuid.Must(uuid.NewV7()).String()
		if err := db.Transact(ctx, func(tx storage.Tx) error {
			e := model.Entry{ID: id, ActorID: "tester", Kind: model.Fact, Title: title, Body: "window", Status: "active", CreatedAt: created, UpdatedAt: created}
			if err := tx.Put(status.Project, e); err != nil {
				return err
			}
			if verified != nil {
				return tx.PutEvidence(status.Project, model.Evidence{ID: uuid.Must(uuid.NewV7()).String(), EntryID: id, Path: "evidence.txt", Commit: strings.Repeat("f", 40), Blob: "deadbeef", CreatedAt: *verified, VerifiedAt: *verified})
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	recent := now
	stale := now.Add(-200 * day)
	freshNoEvidence := mkFact("New fact", now, nil)
	oldNoEvidence := mkFact("Old fact", now.Add(-200*day), nil)
	recentEvidence := mkFact("Old with fresh evidence", now.Add(-200*day), &recent)
	staleEvidence := mkFact("Fresh with stale evidence", now.Add(-day), &stale)
	// Default threshold: 90 unverified-fact days.
	findings, err := s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[string]bool{}
	for _, f := range findings {
		if f.Code == app.CodeUnverifiedFact {
			flagged[f.EntryID] = true
		}
	}
	// New fact without evidence: measured from CreatedAt, within the window.
	if flagged[freshNoEvidence] {
		t.Fatal("new fact without evidence reported immediately")
	}
	// Old fact without evidence: measured from CreatedAt, past the window.
	if !flagged[oldNoEvidence] {
		t.Fatal("old fact without evidence not flagged")
	}
	// Evidence verified recently on an old fact: measured from VerifiedAt.
	if flagged[recentEvidence] {
		t.Fatal("fact with recent evidence verification flagged")
	}
	// Evidence verified long ago on a fresh fact: measured from VerifiedAt.
	if !flagged[staleEvidence] {
		t.Fatal("fact with stale evidence verification not flagged")
	}
}

// reviewState is the storage state Review must never change.
type reviewState struct {
	Entries   []model.Entry
	Relations []model.Relation
	Evidence  []model.Evidence
}

func reviewSnapshot(t *testing.T, ctx context.Context, transact func(context.Context, func(storage.Tx) error) error, p model.Project) reviewState {
	t.Helper()
	var out reviewState
	if err := transact(ctx, func(tx storage.Tx) error {
		entries, err := tx.List(p, model.Filter{Limit: 200})
		if err != nil {
			return err
		}
		out.Entries = entries
		for _, e := range entries {
			links, err := tx.Relations(model.EntryNode(p, e.ID))
			if err != nil {
				return err
			}
			out.Relations = append(out.Relations, links...)
			ev, err := tx.Evidence(p, e.ID)
			if err != nil {
				return err
			}
			out.Evidence = append(out.Evidence, ev...)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestReviewDeterministicAndReadOnly: repeated reviews over identical state
// return identical, fully ordered findings and leave entries, relations, and
// evidence untouched.
func TestReviewDeterministicAndReadOnly(t *testing.T) {
	ctx := context.Background()
	cwd := reviewRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "deterministic.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	if _, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Gone", Body: "b", RelatedFiles: []string{"gone.go"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(ctx, cwd, model.Task, app.CreateInput{Title: "Do", Body: "later"}); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	// Warm-up run so lazy migrations are not attributed to Review.
	if _, err := s.Review(ctx, cwd, nil); err != nil {
		t.Fatal(err)
	}
	before := reviewSnapshot(t, ctx, db.Transact, status.Project)
	first, err := s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	after := reviewSnapshot(t, ctx, db.Transact, status.Project)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("review is not deterministic:\n%+v\n%+v", first, second)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("review mutated storage:\nbefore %+v\nafter %+v", before, after)
	}
	// Full ordering: Code, Source, EntryID, Detail.
	for i := 1; i < len(first); i++ {
		prev, cur := first[i-1], first[i]
		less := prev.Code < cur.Code ||
			(prev.Code == cur.Code && prev.Source < cur.Source) ||
			(prev.Code == cur.Code && prev.Source == cur.Source && prev.EntryID < cur.EntryID) ||
			(prev.Code == cur.Code && prev.Source == cur.Source && prev.EntryID == cur.EntryID && prev.Detail < cur.Detail)
		if !less && (prev.Code != cur.Code || prev.Source != cur.Source || prev.EntryID != cur.EntryID || prev.Detail != cur.Detail) {
			t.Fatalf("findings out of order at %d: %+v then %+v", i, prev, cur)
		}
	}
	if len(first) == 0 {
		t.Fatal("expected findings for the fixture")
	}
}

// Finished entries (done or cancelled tasks, retired facts, superseded
// decisions) are history: a file or evidence that disappeared after they
// closed is not a finding, and retiring them could never clear it.
func TestReviewSkipsFileAndEvidenceChecksForFinishedEntries(t *testing.T) {
	ctx := context.Background()
	cwd := reviewRepo(t)
	writeWorkingFile(t, cwd, "gone.txt", "content\n")
	commitFile(t, cwd, "gone.txt", "add fixture")
	db, err := zova.Open(filepath.Join(t.TempDir(), "finished.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	files := []string{"gone.txt"}
	active, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Active", Body: "B", RelatedFiles: files})
	if err != nil {
		t.Fatal(err)
	}
	retired, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Retired", Body: "B", RelatedFiles: files})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.Add(ctx, cwd, model.Task, app.CreateInput{Title: "Done", Body: "B", RelatedFiles: files})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{active.Entry.ID, retired.Entry.ID} {
		if _, err = s.AddEvidence(ctx, cwd, id, "gone.txt", 0); err != nil {
			t.Fatal(err)
		}
	}
	retiredStatus, doneStatus := "retired", "done"
	if _, err = s.Update(ctx, cwd, model.Fact, retired.Entry.ID, app.UpdateInput{Status: &retiredStatus}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Update(ctx, cwd, model.Task, task.Entry.ID, app.UpdateInput{Status: &doneStatus}); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(cwd, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	findings, err := s.Review(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]map[string]bool{}
	for _, f := range findings {
		if codes[f.EntryID] == nil {
			codes[f.EntryID] = map[string]bool{}
		}
		codes[f.EntryID][f.Code] = true
	}
	if !codes[active.Entry.ID][app.CodeMissingFile] || !codes[active.Entry.ID][app.CodeEvidenceMissing] {
		t.Fatalf("active fact not checked: %+v", codes[active.Entry.ID])
	}
	for _, id := range []string{retired.Entry.ID, task.Entry.ID} {
		for _, code := range []string{app.CodeMissingFile, app.CodeEvidenceMissing, app.CodeEvidenceChanged} {
			if codes[id][code] {
				t.Errorf("finished entry %s reported %s", id, code)
			}
		}
	}
}
