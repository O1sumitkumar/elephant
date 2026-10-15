package app_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage"
	"elephant/internal/storage/zova"
)

func searchRepo(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", p},
		{"-C", p, "remote", "add", "origin", "https://github.com/test/search.git"},
		{"-C", p, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "--allow-empty", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	return p
}

func TestSearchRelatedHistory(t *testing.T) {
	ctx := context.Background()
	cwd := searchRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "search.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	t.Setenv("ELEPHANT_ACTOR_ID", "searcher")
	fact, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Shutdown ownership", Body: "Workers acknowledge cancellation", RelatedFiles: []string{"internal/runtime/worker.go"}})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := s.Add(ctx, cwd, model.Decision, app.CreateInput{Title: "Own cancellation", Body: "Move lifecycle into runtime"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.Add(ctx, cwd, model.Task, app.CreateInput{Title: "Verify shutdown", Body: "Run integration suite", Relations: []app.RelationInput{{Type: "implements", EntryID: dec.Entry.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(ctx, cwd, app.SearchFilters{Query: "shutdown ownership"})
	if err != nil || len(res) == 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if res[0].Match == nil {
		t.Fatal("missing match metadata")
	}
	byActor, err := s.Search(ctx, cwd, app.SearchFilters{Actor: "searcher"})
	if err != nil || len(byActor) != 3 {
		t.Fatalf("%+v %v", byActor, err)
	}
	byFile, err := s.Search(ctx, cwd, app.SearchFilters{File: "internal/runtime/worker.go"})
	if err != nil || len(byFile) != 1 || byFile[0].Entry.ID != fact.Entry.ID {
		t.Fatalf("%+v %v", byFile, err)
	}
	byKind, err := s.Search(ctx, cwd, app.SearchFilters{Kind: model.Task, Query: "shutdown"})
	if err != nil || len(byKind) != 1 {
		t.Fatalf("%+v %v", byKind, err)
	}
	hist, err := s.History(ctx, cwd, "internal/runtime/worker.go", 20)
	if err != nil || len(hist) != 1 || hist[0].ID != fact.Entry.ID {
		t.Fatalf("%+v %v", hist, err)
	}
	if none, err := s.History(ctx, cwd, "never/linked.go", 20); err != nil || len(none) != 0 {
		t.Fatalf("unlinked file history = %+v %v, want empty", none, err)
	}
	rel, err := s.Related(ctx, cwd, task.Entry.ID, "outgoing", "", 1, 20)
	if err != nil || len(rel) != 1 || rel[0].Entry == nil || rel[0].Entry.ID != dec.Entry.ID {
		t.Fatalf("%+v %v", rel, err)
	}
	back, err := s.Related(ctx, cwd, dec.Entry.ID, "incoming", "implements", 1, 20)
	if err != nil || len(back) != 1 {
		t.Fatalf("%+v %v", back, err)
	}
	if _, err = s.Related(ctx, cwd, task.Entry.ID, "sideways", "", 1, 20); err == nil {
		t.Fatal("bad direction accepted")
	}
	if _, err = s.Related(ctx, cwd, task.Entry.ID, "both", "", 9, 20); err == nil {
		t.Fatal("unbounded depth accepted")
	}
}

func TestSearchPerformance(t *testing.T) {
	ctx := context.Background()
	cwd := searchRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "perf.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	for i := range 300 {
		kind := model.Fact
		if i%3 == 1 {
			kind = model.Decision
		} else if i%3 == 2 {
			kind = model.Task
		}
		if _, err = s.Add(ctx, cwd, kind, app.CreateInput{Title: fmt.Sprintf("Entry %03d shutdown", i), Body: "context"}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	res, err := s.Search(ctx, cwd, app.SearchFilters{Query: "shutdown", Limit: 20})
	if err != nil || len(res) != 20 {
		t.Fatalf("%+v %v", res, err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("search too slow: %v", elapsed)
	}
}

func TestSearchFileFilterBeforePagination(t *testing.T) {
	ctx := context.Background()
	cwd := searchRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "filefilter.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	// 30 matching-text entries; only the 20 oldest carry the file relation,
	// so a pre-filtered SQL page would return zero visible matches.
	var withFile []app.SearchResult
	for i := 0; i < 30; i++ {
		files := []string(nil)
		if i < 20 {
			files = []string{"pkg/old.go"}
		}
		r, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: fmt.Sprintf("Paginated %03d", i), Body: "shared needle", RelatedFiles: files})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 1 {
			withFile = append(withFile, app.SearchResult{Entry: r.Entry})
		}
	}
	// Search returns newest first; withFile was appended oldest first.
	for i, j := 0, len(withFile)-1; i < j; i, j = i+1, j-1 {
		withFile[i], withFile[j] = withFile[j], withFile[i]
	}
	// All 20 matches survive pagination on page one.
	res, err := s.Search(ctx, cwd, app.SearchFilters{Query: "needle", File: "pkg/old.go", Limit: 20})
	if err != nil || len(res) != 20 {
		t.Fatalf("len=%d err=%v", len(res), err)
	}
	for i, r := range res {
		if r.Entry.ID != withFile[i].Entry.ID {
			t.Fatalf("order mismatch at %d", i)
		}
		if r.Match == nil || !contains(r.Match, "file") || !contains(r.Match, "body") {
			t.Fatalf("unexpected match metadata: %v", r.Match)
		}
	}
	// Offset skips matching entries, not raw rows.
	page2, err := s.Search(ctx, cwd, app.SearchFilters{Query: "needle", File: "pkg/old.go", Limit: 5, Offset: 20})
	if err != nil || len(page2) != 0 {
		t.Fatalf("offset past end: len=%d err=%v", len(page2), err)
	}
	// Page through a sparse stream: newer entries (with the file) are created
	// after the change below, so exercise offset against the filtered order.
	more, err := s.Search(ctx, cwd, app.SearchFilters{Query: "needle", File: "pkg/old.go", Limit: 3, Offset: 7})
	if err != nil || len(more) != 3 || more[0].Entry.ID != withFile[7].Entry.ID || more[2].Entry.ID != withFile[9].Entry.ID {
		t.Fatalf("offset window mismatch: %+v %v", more, err)
	}
}

func TestSearchUpdatedTimeFilters(t *testing.T) {
	ctx := context.Background()
	cwd := searchRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "timefilter.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	old, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Old fact", Body: "time filter"})
	if err != nil {
		t.Fatal(err)
	}
	new, err := s.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "New fact", Body: "time filter"})
	if err != nil {
		t.Fatal(err)
	}
	if !new.Entry.UpdatedAt.After(old.Entry.UpdatedAt) {
		t.Fatalf("timestamps equal: %v vs %v", old.Entry.UpdatedAt, new.Entry.UpdatedAt)
	}
	between := old.Entry.UpdatedAt.Add(time.Duration(new.Entry.UpdatedAt.Sub(old.Entry.UpdatedAt)/2) + time.Nanosecond)
	after, err := s.Search(ctx, cwd, app.SearchFilters{Query: "time filter", UpdatedAfter: &between})
	if err != nil || len(after) != 1 || after[0].Entry.ID != new.Entry.ID {
		t.Fatalf("updated-after: %+v %v", after, err)
	}
	before, err := s.Search(ctx, cwd, app.SearchFilters{Query: "time filter", UpdatedBefore: &between})
	if err != nil || len(before) != 1 || before[0].Entry.ID != old.Entry.ID {
		t.Fatalf("updated-before: %+v %v", before, err)
	}
	both, err := s.Search(ctx, cwd, app.SearchFilters{Query: "time filter", UpdatedAfter: &old.Entry.UpdatedAt, UpdatedBefore: &new.Entry.UpdatedAt})
	if err != nil || len(both) != 2 {
		t.Fatalf("inclusive window: %+v %v", both, err)
	}
}

func TestSearchCommitRangeFilters(t *testing.T) {
	ctx := context.Background()
	cwd := searchRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "commitrange.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	a, err := s.Add(ctx, cwd, model.Decision, app.CreateInput{Title: "Superseded choice", Body: "commit range"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Entry.StartCommit == nil || *a.Entry.StartCommit == "" {
		t.Fatal("missing start commit")
	}
	// Create a real commit so the supersede boundary is a distinct SHA.
	if err := os.WriteFile(filepath.Join(cwd, "marker.txt"), []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", cwd, "add", ".").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	if out, err := exec.Command("git", "-C", cwd, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "-qm", "boundary").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	b, err := s.Add(ctx, cwd, model.Decision, app.CreateInput{Title: "Current choice", Body: "commit range", Supersedes: a.Entry.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Re-fetch: supersede closes the stored entry's commit range, not the
	// snapshot returned by the original Add.
	a, err = s.Get(ctx, cwd, a.Entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	start := *a.Entry.StartCommit
	end := a.Entry.EndCommit
	if end == nil || *end == "" {
		t.Fatal("supersede did not close the superseded entry's commit range")
	}
	if b.Entry.StartCommit == nil || *b.Entry.StartCommit != *end {
		t.Fatalf("superseding entry should start at the boundary: %+v", b.Entry.StartCommit)
	}
	// Range [start, *end] spans both entries: the superseded entry lives
	// entirely inside it, the superseding entry starts exactly at its end
	// bound (inclusive intersection).
	ranged, err := s.Search(ctx, cwd, app.SearchFilters{CommitStart: start, CommitEnd: *end, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "range composition", ranged, a.Entry.ID, b.Entry.ID)
	// Point range at the boundary commit: the superseded entry ends there
	// (inclusive) and the superseding entry starts there (inclusive).
	point, err := s.Search(ctx, cwd, app.SearchFilters{Commit: *end, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "boundary point", point, a.Entry.ID, b.Entry.ID)
	// Point range at the original start: only the entry still spanning from
	// there is active; the superseding entry starts strictly after it.
	atStart, err := s.Search(ctx, cwd, app.SearchFilters{Commit: start, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "start point", atStart, a.Entry.ID)
	// Open-ended lower bound drops nothing: neither entry ended before
	// start (inclusive).
	fromStart, err := s.Search(ctx, cwd, app.SearchFilters{CommitStart: start, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "open-ended lower bound", fromStart, a.Entry.ID, b.Entry.ID)
	// Match metadata explains the commit hit.
	if m := ranged[0].Match; m == nil || !contains(m, "commit") {
		t.Fatalf("match metadata: %v", m)
	}
}

// sameIDs asserts the search results carry exactly the wanted entry IDs
// (order-insensitive: entries created in one transaction can share a
// timestamp and then tie-break on opaque IDs).
func sameIDs(t *testing.T, label string, res []app.SearchResult, want ...string) {
	t.Helper()
	if len(res) != len(want) {
		t.Fatalf("%s: got %d results %v, want %d", label, len(res), searchIDs(t, res), len(want))
	}
	got := map[string]bool{}
	for _, r := range res {
		got[r.Entry.ID] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("%s: missing %s from %v", label, id, searchIDs(t, res))
		}
	}
}

// mustGit runs git in cwd and fails the test on error.
func mustGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatal(string(out), err)
	}
	return string(out)
}

// TestSearchCommitRangeEndpointOrdering: commit-start must be equal to or an
// ancestor of commit-end. Reversed and divergent endpoints are clear
// validation errors, never accepted as an empty or equality filter.
func TestSearchCommitRangeEndpointOrdering(t *testing.T) {
	ctx := context.Background()
	cwd := searchRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "rangeorder.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	c1 := headSHA(t, cwd)
	c2 := rangeCommit(t, cwd, "c2")
	// Divergent commit: branch from c1 so neither c2 nor side contains the other.
	branch := strings.TrimSpace(mustGit(t, cwd, "rev-parse", "--abbrev-ref", "HEAD"))
	mustGit(t, cwd, "checkout", "-q", "-b", "side", c1)
	side := rangeCommit(t, cwd, "sidechange")
	mustGit(t, cwd, "checkout", "-q", branch)

	expectOK := func(label string, f app.SearchFilters) {
		t.Helper()
		if _, err := s.Search(ctx, cwd, f); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	expectErr := func(label string, f app.SearchFilters, want string) {
		t.Helper()
		res, err := s.Search(ctx, cwd, f)
		if err == nil {
			t.Fatalf("%s: accepted (%d results)", label, len(res))
		}
		if !errors.Is(err, model.ErrInvalidInput) {
			t.Fatalf("%s: not invalid input: %v", label, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: unclear error (want %q): %v", label, want, err)
		}
	}
	// Valid: ancestor, equal endpoints, single-commit shorthand, one-sided.
	expectOK("ancestor range", app.SearchFilters{CommitStart: c1, CommitEnd: c2})
	expectOK("equal range", app.SearchFilters{CommitStart: c2, CommitEnd: c2})
	expectOK("single commit", app.SearchFilters{Commit: c2})
	expectOK("start only", app.SearchFilters{CommitStart: c1})
	expectOK("end only", app.SearchFilters{CommitEnd: c2})
	// Reversed endpoints.
	expectErr("reversed range", app.SearchFilters{CommitStart: c2, CommitEnd: c1}, "reversed")
	// Divergent endpoints are neither orderable nor "reversed".
	expectErr("divergent range", app.SearchFilters{CommitStart: c2, CommitEnd: side}, "divergent")
	expectErr("divergent range swapped", app.SearchFilters{CommitStart: side, CommitEnd: c2}, "divergent")
}

// headSHA returns the current HEAD commit of cwd.
func headSHA(t *testing.T, cwd string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(string(out), err)
	}
	return strings.TrimSpace(string(out))
}

// rangeCommit records a file and returns the new HEAD commit.
func rangeCommit(t *testing.T, cwd, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cwd, name), []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", cwd, "add", ".").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	if out, err := exec.Command("git", "-C", cwd, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "-qm", name).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	return headSHA(t, cwd)
}

func searchIDs(t *testing.T, res []app.SearchResult) []string {
	t.Helper()
	ids := make([]string, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.Entry.ID)
	}
	return ids
}

// TestSearchCommitRangeSemantics pins real Git range behaviour: a requested
// range [start, end] (either bound optional, --commit a single-commit range)
// selects entries whose recorded [start_commit, end_commit] interval
// intersects it. Bounds are inclusive, compared by commit ancestry, and a
// missing entry bound means unbounded (created before history / still open).
// The range filter must compose before limit/offset pagination.
func TestSearchCommitRangeSemantics(t *testing.T) {
	ctx := context.Background()
	cwd := searchRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "rangesem.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	// Linear history c1..c5; searchRepo already created c1.
	c1 := headSHA(t, cwd)
	c2 := rangeCommit(t, cwd, "c2")
	c3 := rangeCommit(t, cwd, "c3")
	c4 := rangeCommit(t, cwd, "c4")
	c5 := rangeCommit(t, cwd, "c5")
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	// Insert entries with crafted commit intervals; all share one timestamp so
	// ordering falls back to the documented id tie-break.
	stamp := time.Now().UTC()
	put := func(id string, start, end *string) {
		t.Helper()
		e := model.Entry{ID: id, Kind: model.Fact, Title: "Range " + id, Body: "commit range semantics", Status: "active",
			StartCommit: start, EndCommit: end, CreatedAt: stamp, UpdatedAt: stamp}
		if err := db.Transact(ctx, func(tx storage.Tx) error { return tx.Put(status.Project, e) }); err != nil {
			t.Fatal(err)
		}
	}
	put("inside", ptr(c2), ptr(c3))        // fully inside [c2,c4]
	put("before", ptr(c1), ptr(c1))        // ended strictly before c2
	put("after", ptr(c5), nil)             // started strictly after c4, still open
	put("overlap-left", ptr(c1), ptr(c3))  // started before, closed inside
	put("overlap-right", ptr(c3), ptr(c5)) // started inside, closed after
	put("containing", ptr(c1), ptr(c5))    // spans the whole requested range
	put("open", ptr(c3), nil)              // opened inside, never closed
	put("touch-start", ptr(c1), ptr(c2))   // ends exactly at the start bound
	put("touch-end", ptr(c4), ptr(c5))     // starts exactly at the end bound
	put("unbounded", nil, nil)             // no recorded commits: spans everything
	// Entry order within a page is updated_at DESC, id DESC; all timestamps are
	// equal here, so ids order the matches: unbounded, touch-start, touch-end,
	// overlap-right, overlap-left, open, inside, containing.
	res, err := s.Search(ctx, cwd, app.SearchFilters{CommitStart: c2, CommitEnd: c4, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := searchIDs(t, res)
	want := []string{"unbounded", "touch-start", "touch-end", "overlap-right", "overlap-left", "open", "inside", "containing"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("range c2..c4 = %v, want %v", got, want)
	}
	if m := res[0].Match; m == nil || !contains(m, "commit") {
		t.Fatalf("match metadata: %v", m)
	}
	// The filter runs before pagination: offset skips matching entries.
	page, err := s.Search(ctx, cwd, app.SearchFilters{CommitStart: c2, CommitEnd: c4, Limit: 50, Offset: 6})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, page); strings.Join(ids, ",") != "inside,containing" {
		t.Fatalf("offset past matches = %v, want [inside containing]", ids)
	}
	// A single-commit range selects entries active at that commit.
	point, err := s.Search(ctx, cwd, app.SearchFilters{Commit: c3, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, point); strings.Join(ids, ",") != "unbounded,overlap-right,overlap-left,open,inside,containing" {
		t.Fatalf("point c3 = %v", ids)
	}
	// Open-ended bounds each drop exactly the entries on the far side.
	lower, err := s.Search(ctx, cwd, app.SearchFilters{CommitStart: c4, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, lower); strings.Join(ids, ",") != "unbounded,touch-end,overlap-right,open,containing,after" {
		t.Fatalf("lower bound c4 = %v", ids)
	}
	upper, err := s.Search(ctx, cwd, app.SearchFilters{CommitEnd: c2, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, upper); strings.Join(ids, ",") != "unbounded,touch-start,overlap-left,inside,containing,before" {
		t.Fatalf("upper bound c2 = %v", ids)
	}
}

// TestSearchCommitRangeRequiresResolvableGitRange: requested bounds must
// resolve to commits in this repository. An unresolvable bound is a clear
// validation error, never a silent equality fallback on start_commit or
// end_commit.
func TestSearchCommitRangeRequiresResolvableGitRange(t *testing.T) {
	ctx := context.Background()
	cwd := searchRepo(t)
	db, err := zova.Open(filepath.Join(t.TempDir(), "unresolved.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	status, err := s.Status(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	// An entry whose recorded start commit is absent from this repository (an
	// imported archive keeps foreign commits). Querying that exact string must
	// fail instead of matching the row by equality.
	foreign := strings.Repeat("ab", 20)
	err = db.Transact(ctx, func(tx storage.Tx) error {
		return tx.Put(status.Project, model.Entry{ID: "foreign", Kind: model.Fact, Title: "Foreign origin", Body: "imported commit", Status: "active", StartCommit: ptr(foreign), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		label string
		f     app.SearchFilters
	}{
		{"unknown start", app.SearchFilters{CommitStart: foreign}},
		{"unknown end", app.SearchFilters{CommitEnd: foreign}},
		{"not a commit", app.SearchFilters{Commit: "not-a-commit"}},
		{"unreachable revision", app.SearchFilters{CommitStart: "HEAD~99"}},
		{"zero object", app.SearchFilters{CommitEnd: strings.Repeat("00000000000000000000000000000000000000000", 1)}},
	}
	for _, tc := range cases {
		res, err := s.Search(ctx, cwd, tc.f)
		if err == nil {
			t.Fatalf("%s: expected error, got %d results", tc.label, len(res))
		}
		if !errors.Is(err, model.ErrInvalidInput) {
			t.Fatalf("%s: not invalid input: %v", tc.label, err)
		}
		if !strings.Contains(err.Error(), "cannot be resolved") {
			t.Fatalf("%s: unclear error: %v", tc.label, err)
		}
	}
	// Resolvable revision syntax (refs, not just full SHAs) is accepted.
	if res, err := s.Search(ctx, cwd, app.SearchFilters{CommitStart: "HEAD"}); err != nil || len(res) != 1 {
		t.Fatalf("resolvable ref: %v %v", searchIDs(t, res), err)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
