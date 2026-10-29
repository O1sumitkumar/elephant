package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	gitrepo "elephant/internal/git"
	"elephant/internal/model"
	"elephant/internal/storage"
)

// InboxGroup groups received additions by source Elephant and project without
// synthesizing canonical state across sources.
type InboxGroup struct {
	SourceElephantID string            `json:"source_elephant_id"`
	ProjectIdentity  string            `json:"project_identity"`
	ProjectName      string            `json:"project_name"`
	Entries          []model.Entry     `json:"entries"`
	Adopted          map[string]string `json:"adopted,omitempty"`
}

// DiffResult compares one remote source table with local state while keeping
// both boundaries explicit.
type DiffResult struct {
	Project     model.Project     `json:"project"`
	Source      model.Project     `json:"source"`
	Local       []model.Entry     `json:"local"`
	Remote      []model.Entry     `json:"remote"`
	Adopted     map[string]string `json:"adopted"`
	RemoteOnly  []model.Entry     `json:"remote_only"`
	Conflicting []model.Entry     `json:"conflicting"`
}

// AdoptResult reports the new local entry plus its preserved provenance.
// Skipped names remote links that were not copied and why.
type AdoptResult struct {
	Entry            model.Entry      `json:"entry"`
	Relations        []model.Relation `json:"relations"`
	SourceElephantID string           `json:"source_elephant_id"`
	SourceEntryID    string           `json:"source_entry_id"`
	AlreadyAdopted   bool             `json:"already_adopted"`
	Skipped          []string         `json:"skipped,omitempty"`
}

func (s *Service) resolveSource(ctx context.Context, tx storage.Tx, selector string) (model.Project, error) {
	if validID(selector) {
		// Source UUID: find its table for the current project is resolved by caller;
		// here list all sources and match.
		sources, err := tx.Sources()
		if err != nil {
			return model.Project{}, err
		}
		for _, src := range sources {
			if src.SourceElephantID == selector {
				return src, nil
			}
		}
		return model.Project{}, fmt.Errorf("%w: unknown source %q", model.ErrInvalidInput, selector)
	}
	r, err := s.Remote(ctx, selector)
	if err != nil {
		return model.Project{}, err
	}
	if r.ElephantID == "" {
		return model.Project{}, fmt.Errorf("%w: remote identity unknown; ensure-project first or use source UUID", model.ErrInvalidInput)
	}
	sources, err := tx.Sources()
	if err != nil {
		return model.Project{}, err
	}
	for _, src := range sources {
		if src.SourceElephantID == r.ElephantID {
			return src, nil
		}
	}
	return model.Project{}, fmt.Errorf("%w: no received state from remote %q", model.ErrInvalidInput, selector)
}

func (s *Service) resolveSourceForProject(tx storage.Tx, identity, selector string, remotes []model.Remote) (model.Project, error) {
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

// Inbox lists received additions grouped by source Elephant and project.
func (s *Service) Inbox(ctx context.Context) (out []InboxGroup, err error) {
	out = []InboxGroup{}
	err = s.store.Transact(ctx, func(tx storage.Tx) error {
		sources, err := tx.Sources()
		if err != nil {
			return err
		}
		for _, src := range sources {
			entries, err := allEntries(tx, src)
			if err != nil {
				return err
			}
			adoptedRows, err := tx.AdoptionsBySource(src.Identity, src.SourceElephantID)
			if err != nil {
				return err
			}
			adopted := map[string]string{}
			for _, a := range adoptedRows {
				adopted[a.SourceEntryID] = a.LocalEntryID
			}
			out = append(out, InboxGroup{SourceElephantID: src.SourceElephantID, ProjectIdentity: src.Identity, ProjectName: src.Name, Entries: entries, Adopted: adopted})
		}
		return nil
	})
	return
}

// sameContent reports whether a remote row duplicates local knowledge: same
// kind, title, and body. Such rows are conflicting rather than new.
func sameContent(a, b model.Entry) bool {
	return a.Kind == b.Kind && a.Title == b.Title && a.Body == b.Body
}

// Diff compares a remote source table with the local project state.
func (s *Service) Diff(ctx context.Context, cwd, selector string) (out DiffResult, err error) {
	g, err := gitrepo.Inspect(ctx, cwd)
	if err != nil {
		return out, err
	}
	err = s.store.Transact(ctx, func(tx storage.Tx) error {
		local, err := resolve(tx, g)
		if err != nil {
			return err
		}
		remotes, err := tx.Remotes()
		if err != nil {
			return err
		}
		src, err := s.resolveSourceForProject(tx, g.Identity, selector, remotes)
		if err != nil {
			return err
		}
		out.Project = local
		out.Source = src
		out.Local = []model.Entry{}
		out.Remote = []model.Entry{}
		out.Adopted = map[string]string{}
		out.RemoteOnly = []model.Entry{}
		out.Conflicting = []model.Entry{}
		if out.Local, err = allEntries(tx, local); err != nil {
			return err
		}
		if out.Remote, err = allEntries(tx, src); err != nil {
			return err
		}
		adoptedRows, err := tx.AdoptionsBySource(g.Identity, src.SourceElephantID)
		if err != nil {
			return err
		}
		for _, a := range adoptedRows {
			out.Adopted[a.SourceEntryID] = a.LocalEntryID
		}
		for _, e := range out.Remote {
			if _, ok := out.Adopted[e.ID]; ok {
				continue
			}
			duplicate := false
			for _, l := range out.Local {
				if sameContent(e, l) {
					duplicate = true
					break
				}
			}
			if duplicate {
				out.Conflicting = append(out.Conflicting, e)
			} else {
				out.RemoteOnly = append(out.RemoteOnly, e)
			}
		}
		return nil
	})
	return
}

// resolveLocalFile validates a repository-relative remote file path against
// the local repository. It rejects anything CleanFile rejects, requires the
// target to exist, and resolves symlinks so a link pointing outside the
// repository (a symlink escape) is also rejected. The returned path stays in
// slash-separated repository-relative form.
func resolveLocalFile(root, file string) (string, error) {
	clean, err := model.CleanFile(file)
	if err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(rootAbs, filepath.FromSlash(clean)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q resolves outside the repository", file)
	}
	return filepath.ToSlash(rel), nil
}

// Adopt creates a new local entry from a received remote entry, preserving
// provenance and remaining idempotent on repeat. File links are copied only
// when the path is valid and resolves to an existing file inside the local
// repository; entry relations are copied only through prior adoption mappings
// or validated local targets. Anything else is skipped and reported. Every
// storage error aborts the transaction, rolling back the new entry, copied
// links, and the provenance mapping together.
func (s *Service) Adopt(ctx context.Context, cwd, entryID, selector string) (out AdoptResult, err error) {
	g, err := gitrepo.Inspect(ctx, cwd)
	if err != nil {
		return out, err
	}
	err = s.store.Transact(ctx, func(tx storage.Tx) error {
		local, err := resolve(tx, g)
		if err != nil {
			return err
		}
		// Resolve inside the same transaction to keep boundaries consistent.
		remotes, err := tx.Remotes()
		if err != nil {
			return err
		}
		src, err := s.resolveSourceForProject(tx, g.Identity, selector, remotes)
		if err != nil {
			return err
		}
		if existing, err := tx.Adoption(g.Identity, src.SourceElephantID, entryID); err == nil {
			e, err := tx.Get(local, existing.LocalEntryID)
			if err != nil {
				return err
			}
			links, err := tx.Relations(model.EntryNode(local, e.ID))
			if err != nil {
				return err
			}
			out = AdoptResult{Entry: e, Relations: links, SourceElephantID: src.SourceElephantID, SourceEntryID: entryID, AlreadyAdopted: true}
			return nil
		}
		remote, err := tx.Get(src, entryID)
		if err != nil {
			return err
		}
		remoteLinks, err := tx.Relations(model.EntryNode(src, remote.ID))
		if err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		// The adopted entry keeps its lifecycle state, except that a decision
		// is superseded locally only through a supersedes edge: adopting its
		// replacement marks it.
		status := remote.Status
		if remote.Kind == model.Decision && status == "superseded" {
			status = "active"
		}
		e := model.Entry{ID: id.String(), ActorID: currentActor(), Kind: remote.Kind, Title: remote.Title, Body: remote.Body, Status: status, TargetVersion: remote.TargetVersion, StartCommit: optional(g.Head), CreatedAt: now, UpdatedAt: now}
		if model.Terminal(e.Kind, e.Status) {
			e.EndCommit = optional(g.Head)
		}
		if err = e.Validate(); err != nil {
			return err
		}
		if err = tx.Put(local, e); err != nil {
			return err
		}
		// Copy file links only when the path resolves safely inside the local
		// repository; copy entry relations only through prior adoption mappings
		// or validated local targets. Anything else is skipped and reported.
		adoptedBySource := map[string]string{}
		prior, err := tx.AdoptionsBySource(g.Identity, src.SourceElephantID)
		if err != nil {
			return err
		}
		for _, a := range prior {
			adoptedBySource[a.SourceEntryID] = a.LocalEntryID
		}
		var skipped []string
		for _, r := range remoteLinks {
			if strings.HasPrefix(r.To, "file:"+src.ID+":") {
				file := strings.TrimPrefix(r.To, "file:"+src.ID+":")
				clean, err := resolveLocalFile(g.Root, file)
				if err != nil {
					skipped = append(skipped, fmt.Sprintf("file %q: %v", file, err))
					continue
				}
				if err = tx.Link(local, model.Relation{From: model.EntryNode(local, e.ID), Type: model.FileEdge(e.Kind), To: "file:" + local.ID + ":" + clean}); err != nil {
					return err
				}
				continue
			}
			// Entry edge: resolve the raw entry ID suffix and map it through
			// prior adoptions, else through validated local targets.
			target := r.To
			if i := strings.LastIndex(target, ":"); i >= 0 {
				target = target[i+1:]
			}
			localTarget, ok := adoptedBySource[target]
			if !ok {
				other, err := tx.Get(local, target)
				if err != nil {
					skipped = append(skipped, fmt.Sprintf("relation %s: no local target for %q", r.Type, target))
					continue
				}
				if err = model.ValidateRelation(e.Kind, r.Type, other.Kind); err != nil {
					skipped = append(skipped, fmt.Sprintf("relation %s: incompatible local target", r.Type))
					continue
				}
				localTarget = other.ID
			} else {
				other, err := tx.Get(local, localTarget)
				if err != nil {
					skipped = append(skipped, fmt.Sprintf("relation %s: adopted target is gone", r.Type))
					continue
				}
				if err = model.ValidateRelation(e.Kind, r.Type, other.Kind); err != nil {
					skipped = append(skipped, fmt.Sprintf("relation %s: incompatible adopted target", r.Type))
					continue
				}
			}
			if r.Type == "supersedes" {
				// Same rule as a local supersede: only an active decision can
				// be replaced, and replacing it marks it superseded.
				old, err := tx.Get(local, localTarget)
				if err != nil {
					return err
				}
				if old.Status != "active" {
					skipped = append(skipped, fmt.Sprintf("relation supersedes: local decision %s is %s, not active", old.ID, old.Status))
					continue
				}
				old.Status = "superseded"
				old.EndCommit = optional(g.Head)
				old.UpdatedAt = now
				if err = tx.Put(local, old); err != nil {
					return err
				}
			}
			if err = tx.Link(local, model.Relation{From: model.EntryNode(local, e.ID), Type: r.Type, To: model.EntryNode(local, localTarget)}); err != nil {
				return err
			}
		}
		links, err := tx.Relations(model.EntryNode(local, e.ID))
		if err != nil {
			return err
		}
		adoptID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if err = tx.RecordAdoption(model.Adoption{ID: adoptID.String(), ProjectIdentity: g.Identity, SourceElephantID: src.SourceElephantID, SourceEntryID: remote.ID, LocalEntryID: e.ID, Kind: string(e.Kind), CreatedAt: now}); err != nil {
			return err
		}
		out = AdoptResult{Entry: e, Relations: links, SourceElephantID: src.SourceElephantID, SourceEntryID: remote.ID, Skipped: skipped}
		return nil
	})
	return
}
