package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	gitrepo "elephant/internal/git"
	"elephant/internal/model"
	"elephant/internal/storage"
)

// SearchResult pairs an entry with deterministic match metadata explaining why
// it matched (title, body, actor, file, relation, etc.).
type SearchResult struct {
	Entry model.Entry `json:"entry"`
	Match []string    `json:"match"`
}

// Search filters supported by CLI and MCP. Source selects a remote source
// table; empty means the local project. Commit, CommitStart and CommitEnd
// request an inclusive Git commit range resolved against the local
// repository; Commit is the single-commit shorthand for a range of one.
type SearchFilters struct {
	Query         string
	Kind          model.Kind
	Status        string
	Actor         string
	TargetVersion string
	Commit        string
	CommitStart   string
	CommitEnd     string
	File          string
	Source        string
	UpdatedAfter  *time.Time
	UpdatedBefore *time.Time
	Limit         int
	Offset        int
}

// Search runs deterministic text retrieval scoped to one project table.
// searchPage is the bounded batch size used to walk a project table when a
// filter that the store cannot apply (repository-relative file membership,
// commit-range ancestry) must run before pagination. The scan stops as soon
// as a full page of matches is collected, so unfiltered queries pay no extra
// cost.
const searchPage = 40

// commitRange is a requested commit interval with inclusive, optional
// bounds, already resolved to full commit SHAs in the local repository.
type commitRange struct {
	start string // "" means unbounded below
	end   string // "" means unbounded above
}

// commitMatcher decides whether an entry's recorded commit interval
// intersects the requested range. Recorded-revision resolutions and ancestry
// answers are cached so each distinct stored commit costs at most a few Git
// calls per search.
type commitMatcher struct {
	root      string
	requested commitRange
	resolved  map[string]string // recorded revision -> full SHA, "" when absent
	ancestor  map[string]bool   // "a\x00b" -> a is an ancestor of b
}

// newCommitMatcher resolves the commit filters against root. An unresolvable
// bound is a validation error: a range endpoint is never silently downgraded
// to an equality filter. It returns nil when no commit filter is set.
func newCommitMatcher(ctx context.Context, root string, f SearchFilters) (*commitMatcher, error) {
	// --commit C is the single-commit range [C, C]; an explicit
	// --commit-start/--commit-end replaces that side of the range.
	start, startLabel := f.CommitStart, "commit range start"
	end, endLabel := f.CommitEnd, "commit range end"
	if start == "" && f.Commit != "" {
		start, startLabel = f.Commit, "commit"
	}
	if end == "" && f.Commit != "" {
		end, endLabel = f.Commit, "commit"
	}
	if start == "" && end == "" {
		return nil, nil
	}
	m := &commitMatcher{root: root, resolved: map[string]string{}, ancestor: map[string]bool{}}
	for _, bound := range []struct {
		label string
		rev   string
		dst   *string
	}{
		{startLabel, start, &m.requested.start},
		{endLabel, end, &m.requested.end},
	} {
		if bound.rev == "" {
			continue
		}
		sha, err := gitrepo.ResolveCommit(ctx, root, bound.rev)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, gitrepo.ErrUnknownCommit) {
				return nil, fmt.Errorf("%w: %s %q cannot be resolved to a commit in this repository", model.ErrInvalidInput, bound.label, bound.rev)
			}
			return nil, err
		}
		*bound.dst = sha
	}
	// The range must run forward: the start bound has to be equal to or an
	// ancestor of the end bound. Reversed or divergent endpoints cannot
	// define an interval and are a clear validation error.
	if m.requested.start != "" && m.requested.end != "" {
		forward, err := gitrepo.IsAncestor(ctx, root, m.requested.start, m.requested.end)
		if err != nil {
			return nil, err
		}
		if !forward {
			reversed, err := gitrepo.IsAncestor(ctx, root, m.requested.end, m.requested.start)
			if err != nil {
				return nil, err
			}
			if reversed {
				return nil, fmt.Errorf("%w: commit range is reversed: start %q is a descendant of end %q", model.ErrInvalidInput, start, end)
			}
			return nil, fmt.Errorf("%w: commit range endpoints %q and %q are on divergent histories", model.ErrInvalidInput, start, end)
		}
	}
	return m, nil
}

// recordedSHA caches the full SHA of a stored commit, "" when this
// repository does not carry it.
func (m *commitMatcher) recordedSHA(ctx context.Context, rev string) (string, error) {
	if sha, ok := m.resolved[rev]; ok {
		return sha, nil
	}
	sha, err := gitrepo.ResolveCommit(ctx, m.root, rev)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !errors.Is(err, gitrepo.ErrUnknownCommit) {
			return "", err
		}
		sha = ""
	}
	m.resolved[rev] = sha
	return sha, nil
}

// precedes reports whether a sits strictly before b on one line of history.
// Commits on divergent histories do not precede each other.
func (m *commitMatcher) precedes(ctx context.Context, a, b string) (bool, error) {
	if a == b {
		return false, nil
	}
	key := a + "\x00" + b
	if known, ok := m.ancestor[key]; ok {
		return known, nil
	}
	before, err := gitrepo.IsAncestor(ctx, m.root, a, b)
	if err != nil {
		return false, err
	}
	m.ancestor[key] = before
	return before, nil
}

// matches reports whether the entry's recorded commit interval intersects the
// requested range. An entry is excluded only when Git proves it lies entirely
// outside: a missing bound (created before the repository had commits, or
// still open), a bound this repository cannot read (an imported archive keeps
// foreign commits), and commits on divergent histories cannot prove exclusion
// and therefore keep the entry.
func (m *commitMatcher) matches(ctx context.Context, e model.Entry) (bool, error) {
	if m.requested.start != "" && e.EndCommit != nil && *e.EndCommit != "" {
		end, err := m.recordedSHA(ctx, *e.EndCommit)
		if err != nil {
			return false, err
		}
		if end != "" {
			// Ended strictly before the range started.
			before, err := m.precedes(ctx, end, m.requested.start)
			if err != nil {
				return false, err
			}
			if before {
				return false, nil
			}
		}
	}
	if m.requested.end != "" && e.StartCommit != nil && *e.StartCommit != "" {
		start, err := m.recordedSHA(ctx, *e.StartCommit)
		if err != nil {
			return false, err
		}
		if start != "" {
			// Started strictly after the range ended.
			before, err := m.precedes(ctx, m.requested.end, start)
			if err != nil {
				return false, err
			}
			if before {
				return false, nil
			}
		}
	}
	return true, nil
}

// Search runs deterministic text retrieval scoped to one project table.
// The file and commit-range filters compose before limit/offset pagination:
// matching entries keep their position in the deterministic ordering instead
// of being lost to a SQL page that was sliced before the filter ran.
func (s *Service) Search(ctx context.Context, cwd string, f SearchFilters) (out []SearchResult, err error) {
	out = []SearchResult{}
	if f.Kind != "" && f.Kind != model.Fact && f.Kind != model.Decision && f.Kind != model.Task {
		return nil, model.ErrInvalidKind
	}
	if f.Status != "" && !model.ValidStatus(f.Kind, f.Status) {
		return nil, model.ErrInvalidStatus
	}
	if f.Limit < 0 || f.Limit > 50 || f.Offset < 0 {
		return nil, fmt.Errorf("%w: limit must be 0–50 and offset nonnegative", model.ErrInvalidInput)
	}
	limit := f.Limit
	if limit == 0 {
		limit = 20
	}
	lower := strings.ToLower(strings.TrimSpace(f.Query))
	g, err := gitrepo.Inspect(ctx, cwd)
	if err != nil {
		return nil, err
	}
	var cleanFile string
	if f.File != "" {
		if cleanFile, err = repoFile(g, f.File); err != nil {
			return nil, err
		}
	}
	// Resolve requested range bounds up front: an unresolvable bound must
	// fail the query before any entry is examined.
	matcher, err := newCommitMatcher(ctx, g.Root, f)
	if err != nil {
		return nil, err
	}
	err = s.store.Transact(ctx, func(tx storage.Tx) error {
		local, err := resolve(tx, g)
		if err != nil {
			return err
		}
		project := local
		if f.Source != "" {
			remotes, err := tx.Remotes()
			if err != nil {
				return err
			}
			project, err = s.resolveSourceForProjectLite(tx, g.Identity, f.Source, remotes)
			if err != nil {
				return err
			}
		}
		// getMatches returns one bounded page of entries, applying the file
		// membership filter and the commit-range filter in the app layer
		// where the store cannot (relations and Git ancestry live outside
		// SQL). The second result reports whether the underlying store page
		// was the last one (fewer than searchPage rows), which terminates
		// the walk.
		getMatches := func(offset int) ([]model.Entry, bool, error) {
			entries, err := tx.Search(project, model.SearchQuery{
				Query: f.Query, Kind: f.Kind, Status: f.Status, Actor: f.Actor,
				TargetVersion: f.TargetVersion,
				UpdatedAfter:  f.UpdatedAfter, UpdatedBefore: f.UpdatedBefore,
				Limit: searchPage, Offset: offset,
			})
			if err != nil {
				return nil, false, err
			}
			exhausted := len(entries) < searchPage
			if cleanFile == "" && matcher == nil {
				return entries, exhausted, nil
			}
			filtered := make([]model.Entry, 0, len(entries))
			for _, e := range entries {
				if matcher != nil {
					keep, err := matcher.matches(ctx, e)
					if err != nil {
						return nil, false, err
					}
					if !keep {
						continue
					}
				}
				if cleanFile == "" {
					filtered = append(filtered, e)
					continue
				}
				links, err := tx.Relations(model.EntryNode(project, e.ID))
				if err != nil {
					return nil, false, err
				}
				for _, r := range links {
					if r.To == "file:"+project.ID+":"+cleanFile {
						filtered = append(filtered, e)
						break
					}
				}
			}
			return filtered, exhausted, nil
		}
		// Pagination always applies to the filtered stream: offset skips
		// matching entries, limit bounds returned matches, regardless of
		// whether the app-layer filters narrowed the store rows first.
		skip := f.Offset
		collected := 0
		offset := 0
		for collected < skip+limit {
			entries, exhausted, err := getMatches(offset)
			if err != nil {
				return err
			}
			if len(entries) == 0 && exhausted {
				break
			}
			for _, e := range entries {
				match := []string{}
				if lower != "" {
					if strings.Contains(strings.ToLower(e.Title), lower) {
						match = append(match, "title")
					}
					if strings.Contains(strings.ToLower(e.Body), lower) {
						match = append(match, "body")
					}
				}
				if f.Actor != "" && e.ActorID == f.Actor {
					match = append(match, "actor")
				}
				if f.TargetVersion != "" {
					match = append(match, "target_version")
				}
				if f.Commit != "" || f.CommitStart != "" || f.CommitEnd != "" {
					match = append(match, "commit")
				}
				if cleanFile != "" {
					match = append(match, "file")
				}
				if len(match) == 0 {
					match = []string{"filter"}
				}
				sort.Strings(match)
				if collected < skip {
					collected++
					continue
				}
				out = append(out, SearchResult{Entry: e, Match: match})
				collected++
				if collected >= skip+limit {
					break
				}
			}
			offset += searchPage
			if exhausted {
				break
			}
		}
		return nil
	})
	return
}

func (s *Service) resolveSourceForProjectLite(tx storage.Tx, identity, selector string, remotes []model.Remote) (model.Project, error) {
	if validID(selector) {
		return tx.Source(identity, selector)
	}
	for _, r := range remotes {
		if r.Name == selector {
			if r.ElephantID == "" {
				return model.Project{}, fmt.Errorf("%w: remote identity unknown; ensure-project first or use source UUID", model.ErrInvalidInput)
			}
			return tx.Source(identity, r.ElephantID)
		}
	}
	return model.Project{}, fmt.Errorf("%w: unknown remote %q", model.ErrInvalidInput, selector)
}

// RelatedResult is one graph neighborhood hit with its depth.
type RelatedResult struct {
	Entry    *model.Entry   `json:"entry,omitempty"`
	Node     string         `json:"node"`
	Edge     string         `json:"edge"`
	Depth    int            `json:"depth"`
	Relation model.Relation `json:"relation"`
}

// Related performs bounded graph traversal from one entry. Direction is
// outgoing, incoming, or both; edge filters a single edge type; depth is
// 1–3; limit is 1–50. Traversal never crosses project or remote table
// ownership: only entry: and file: nodes of the same project are followed.
func (s *Service) Related(ctx context.Context, cwd, entryID, direction, edge string, depth, limit int) (out []RelatedResult, err error) {
	out = []RelatedResult{}
	if direction == "" {
		direction = "both"
	}
	if direction != "outgoing" && direction != "incoming" && direction != "both" {
		return nil, fmt.Errorf("%w: direction must be outgoing, incoming, or both", model.ErrInvalidInput)
	}
	if depth <= 0 || depth > 3 {
		return nil, fmt.Errorf("%w: depth must be 1–3", model.ErrInvalidInput)
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	g, err := gitrepo.Inspect(ctx, cwd)
	if err != nil {
		return nil, err
	}
	err = s.store.Transact(ctx, func(tx storage.Tx) error {
		project, err := resolve(tx, g)
		if err != nil {
			return err
		}
		if _, err = tx.Get(project, entryID); err != nil {
			return err
		}
		start := model.EntryNode(project, entryID)
		type item struct {
			node  string
			depth int
		}
		visited := map[string]bool{start: true}
		queue := []item{{start, 0}}
		for len(queue) > 0 && len(out) < limit {
			cur := queue[0]
			queue = queue[1:]
			if cur.depth >= depth {
				continue
			}
			var edges []model.Relation
			if direction == "outgoing" || direction == "both" {
				links, err := tx.Relations(cur.node)
				if err != nil {
					return err
				}
				for _, r := range links {
					edges = append(edges, model.Relation{From: cur.node, Type: r.Type, To: r.To})
				}
			}
			if direction == "incoming" || direction == "both" {
				links, err := tx.Incoming(cur.node)
				if err != nil {
					return err
				}
				edges = append(edges, links...)
			}
			sort.Slice(edges, func(i, j int) bool {
				if edges[i].Type != edges[j].Type {
					return edges[i].Type < edges[j].Type
				}
				if edges[i].From != edges[j].From {
					return edges[i].From < edges[j].From
				}
				return edges[i].To < edges[j].To
			})
			for _, r := range edges {
				if edge != "" && r.Type != edge {
					continue
				}
				next := r.To
				if next == cur.node {
					next = r.From
				} else if direction == "incoming" && r.To == cur.node {
					next = r.From
				} else if direction == "outgoing" && r.From == cur.node {
					next = r.To
				} else if direction == "both" {
					if r.From == cur.node {
						next = r.To
					} else {
						next = r.From
					}
				}
				// Ownership boundary: only same-project entry/file nodes.
				if !inProject(project, next) {
					continue
				}
				if visited[next] {
					continue
				}
				visited[next] = true
				rec := RelatedResult{Node: next, Edge: r.Type, Depth: cur.depth + 1, Relation: r}
				if eid, ok := projectEntryID(project, next); ok {
					if e, err := tx.Get(project, eid); err == nil {
						c := e
						rec.Entry = &c
					}
				}
				out = append(out, rec)
				if len(out) >= limit {
					break
				}
				queue = append(queue, item{next, cur.depth + 1})
			}
		}
		return nil
	})
	return
}

func inProject(p model.Project, node string) bool {
	if after, ok := strings.CutPrefix(node, "entry:"); ok {
		if strings.Contains(after, ":") {
			// Remote-scoped form entry:<table>:<id>.
			return strings.HasPrefix(after, p.TableName+":")
		}
		return p.Scope == "local"
	}
	if after, ok := strings.CutPrefix(node, "file:"); ok {
		return strings.HasPrefix(after, p.ID+":")
	}
	return false
}

func projectEntryID(p model.Project, node string) (string, bool) {
	after, ok := strings.CutPrefix(node, "entry:")
	if !ok {
		return "", false
	}
	if i := strings.LastIndex(after, ":"); i >= 0 {
		after = after[i+1:]
	}
	if !validID(after) {
		return "", false
	}
	return after, true
}

// History lists entries linked to one repository-relative file, newest first.
func (s *Service) History(ctx context.Context, cwd, file string, limit int) (out []model.Entry, err error) {
	out = []model.Entry{}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	g, err := gitrepo.Inspect(ctx, cwd)
	if err != nil {
		return nil, err
	}
	clean, err := repoFile(g, file)
	if err != nil {
		return nil, err
	}
	err = s.store.Transact(ctx, func(tx storage.Tx) error {
		project, err := resolve(tx, g)
		if err != nil {
			return err
		}
		if out, err = fileEntries(tx, project, clean); err != nil {
			return err
		}
		if len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	return
}

// fileEntries lists this project's entries linked to one clean
// repository-relative file, newest first.
func fileEntries(tx storage.Tx, project model.Project, clean string) ([]model.Entry, error) {
	out := []model.Entry{}
	incoming, err := tx.Incoming("file:" + project.ID + ":" + clean)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range incoming {
		eid, ok := projectEntryID(project, r.From)
		if !ok || seen[eid] {
			continue
		}
		seen[eid] = true
		e, err := tx.Get(project, eid)
		if err != nil {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}
