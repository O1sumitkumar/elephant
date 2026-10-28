// Package app implements project continuity without depending on an agent protocol.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	gitrepo "elephant/internal/git"
	"elephant/internal/model"
	"elephant/internal/storage"
)

type Service struct{ store storage.Store }

func New(s storage.Store) *Service { return &Service{store: s} }

type RelationInput struct {
	Type    string `json:"type"`
	EntryID string `json:"entry_id"`
}

// EvidenceInput attaches local evidence to a remote send by entry ID.
type EvidenceInput struct {
	EntryID string `json:"entry_id"`
}

type CreateInput struct {
	Title         string          `json:"title"`
	Body          string          `json:"body"`
	TargetVersion *string         `json:"target_version,omitempty"`
	RelatedFiles  []string        `json:"related_files,omitempty"`
	Relations     []RelationInput `json:"relations,omitempty"`
	Evidence      []EvidenceInput `json:"evidence,omitempty"`
	Supersedes    string          `json:"supersedes,omitempty"`
}
type UpdateInput struct {
	Title         *string         `json:"title,omitempty"`
	Body          *string         `json:"body,omitempty"`
	Status        *string         `json:"status,omitempty"`
	TargetVersion *string         `json:"target_version,omitempty"`
	RelatedFiles  *[]string       `json:"related_files,omitempty"`
	Relations     []RelationInput `json:"relations,omitempty"`
}

// Record is one entry with its graph context: outgoing relations, incoming
// relations from this project's entries and checkpoints, and stored evidence
// rows (use evidence verify for their current state).
type Record struct {
	Entry     model.Entry      `json:"entry"`
	Relations []model.Relation `json:"relations"`
	Incoming  []model.Relation `json:"incoming"`
	Evidence  []model.Evidence `json:"evidence"`
}
type Status struct {
	Project model.Project    `json:"project"`
	Git     gitrepo.Metadata `json:"git"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func resolve(tx storage.Tx, g gitrepo.Metadata) (model.Project, error) {
	p, err := tx.Project(g.Identity)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, model.ErrProjectNotFound) {
		return p, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return p, err
	}
	now := time.Now().UTC()
	p = model.Project{ID: "prj_" + id.String(), Identity: g.Identity, Name: g.Name, Remote: g.Remote, TableName: "p_" + strings.ReplaceAll(id.String(), "-", ""), CreatedAt: now, UpdatedAt: now}
	p.Scope = "local"
	p.SourceElephantID, err = tx.Identity()
	if err != nil {
		return p, err
	}
	return p, tx.CreateProject(p)
}
func (s *Service) within(ctx context.Context, cwd string, fn func(storage.Tx, model.Project, gitrepo.Metadata) error) error {
	g, err := gitrepo.Inspect(ctx, cwd)
	if err != nil {
		return err
	}
	return s.store.Transact(ctx, func(tx storage.Tx) error {
		p, err := resolve(tx, g)
		if err != nil {
			return err
		}
		return fn(tx, p, g)
	})
}
func (s *Service) Status(ctx context.Context, cwd string) (out Status, err error) {
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error { out = Status{p, g}; return nil })
	return
}
func record(tx storage.Tx, p model.Project, e model.Entry) (Record, error) {
	out := Record{Entry: e, Incoming: []model.Relation{}}
	var err error
	if out.Relations, err = tx.Relations("entry:" + e.ID); err != nil {
		return out, err
	}
	incoming, err := tx.Incoming(model.EntryNode(p, e.ID))
	if err != nil {
		return out, err
	}
	for _, r := range incoming {
		if inProject(p, r.From) || strings.HasPrefix(r.From, "checkpoint:"+p.TableName+":") {
			out.Incoming = append(out.Incoming, r)
		}
	}
	if out.Evidence, err = tx.Evidence(p, e.ID); err != nil {
		return out, err
	}
	if out.Evidence == nil {
		out.Evidence = []model.Evidence{}
	}
	return out, nil
}
func (s *Service) Get(ctx context.Context, cwd, id string) (out Record, err error) {
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error {
		e, err := tx.Get(p, id)
		if err != nil {
			return err
		}
		out, err = record(tx, p, e)
		return err
	})
	return
}
func (s *Service) List(ctx context.Context, cwd string, f model.Filter) (out []model.Entry, err error) {
	if f.Kind != "" && f.Kind != model.Fact && f.Kind != model.Decision && f.Kind != model.Task {
		return nil, model.ErrInvalidKind
	}
	if f.Status != "" && !model.ValidStatus(f.Kind, f.Status) {
		return nil, model.ErrInvalidStatus
	}
	if f.Limit < 0 || f.Limit > 200 || f.Offset < 0 {
		return nil, fmt.Errorf("%w: limit must be 0–200 and offset nonnegative", model.ErrInvalidInput)
	}
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error {
		var err error
		out, err = tx.List(p, f)
		return err
	})
	return
}
func (s *Service) Add(ctx context.Context, cwd string, k model.Kind, in CreateInput) (out Record, err error) {
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		e := model.Entry{ID: id.String(), ActorID: currentActor(), Kind: k, Title: in.Title, Body: in.Body, Status: model.DefaultStatus(k), TargetVersion: in.TargetVersion, StartCommit: optional(g.Head), CreatedAt: now, UpdatedAt: now}
		if err = e.Validate(); err != nil {
			return err
		}
		files, err := repoFiles(g, in.RelatedFiles)
		if err != nil {
			return err
		}
		if err = tx.Put(p, e); err != nil {
			return err
		}
		if err = attach(tx, p, e, files, in.Relations); err != nil {
			return err
		}
		if in.Supersedes != "" {
			if k != model.Decision {
				return model.ErrInvalidKind
			}
			old, err := tx.Get(p, in.Supersedes)
			if err != nil {
				return err
			}
			if old.Kind != model.Decision {
				return model.ErrInvalidKind
			}
			if old.Status != "active" {
				return model.ErrInvalidTransition
			}
			old.Status = "superseded"
			old.EndCommit = optional(g.Head)
			old.UpdatedAt = now
			if err = tx.Put(p, old); err != nil {
				return err
			}
			if err = tx.Link(p, model.Relation{From: "entry:" + e.ID, Type: "supersedes", To: "entry:" + old.ID}); err != nil {
				return err
			}
		}
		out, err = record(tx, p, e)
		return err
	})
	return
}
func (s *Service) Update(ctx context.Context, cwd string, k model.Kind, id string, in UpdateInput) (out Record, err error) {
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error {
		e, err := tx.Get(p, id)
		if err != nil {
			return err
		}
		if e.Kind != k {
			return model.ErrInvalidKind
		}
		if in.Title != nil {
			e.Title = *in.Title
		}
		if in.Body != nil {
			e.Body = *in.Body
		}
		if in.TargetVersion != nil {
			e.TargetVersion = optional(*in.TargetVersion)
		}
		if in.Status != nil {
			if *in.Status == "superseded" && e.Status != "superseded" {
				return fmt.Errorf("%w: use supersede_decision with a replacement", model.ErrInvalidTransition)
			}
			if err = model.ValidateTransition(k, e.Status, *in.Status); err != nil {
				return err
			}
			if e.Status != *in.Status && model.Terminal(k, *in.Status) {
				e.EndCommit = optional(g.Head)
			}
			e.Status = *in.Status
		}
		e.UpdatedAt = time.Now().UTC()
		if err = e.Validate(); err != nil {
			return err
		}
		if err = tx.Put(p, e); err != nil {
			return err
		}
		var files []string
		if in.RelatedFiles != nil {
			links, err := tx.Relations("entry:" + id)
			if err != nil {
				return err
			}
			for _, r := range links {
				if r.Type == model.FileEdge(k) {
					if err = tx.Unlink(r); err != nil {
						return err
					}
				}
			}
			if files, err = repoFiles(g, *in.RelatedFiles); err != nil {
				return err
			}
		}
		if err = attach(tx, p, e, files, in.Relations); err != nil {
			return err
		}
		out, err = record(tx, p, e)
		return err
	})
	return
}

// repoFile resolves a file argument, given relative to the caller's working
// directory like a Git pathspec, to a clean repository-relative path.
func repoFile(g gitrepo.Metadata, file string) (string, error) {
	if path.IsAbs(file) || path.Clean(file) == "." {
		return model.CleanFile(file) // rejected as not repository-relative
	}
	return model.CleanFile(path.Join(g.Prefix, file))
}
func repoFiles(g gitrepo.Metadata, files []string) ([]string, error) {
	if len(files) == 0 {
		return files, nil
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		clean, err := repoFile(g, f)
		if err != nil {
			return nil, err
		}
		out = append(out, clean)
	}
	return out, nil
}
func attach(tx storage.Tx, p model.Project, e model.Entry, files []string, links []RelationInput) error {
	if len(files)+len(links) > 99 {
		return fmt.Errorf("%w: at most 99 related files and entries per mutation", model.ErrInvalidInput)
	}
	for _, f := range files {
		clean, err := model.CleanFile(f)
		if err != nil {
			return err
		}
		if err = tx.Link(p, model.Relation{From: "entry:" + e.ID, Type: model.FileEdge(e.Kind), To: "file:" + p.ID + ":" + clean}); err != nil {
			return err
		}
	}
	for _, r := range links {
		if r.Type == "supersedes" {
			return fmt.Errorf("%w: use the supersedes field", model.ErrInvalidInput)
		}
		if r.EntryID == e.ID {
			return fmt.Errorf("%w: cannot link an entry to itself", model.ErrInvalidInput)
		}
		other, err := tx.Get(p, r.EntryID)
		if err != nil {
			return err
		}
		if err = model.ValidateRelation(e.Kind, r.Type, other.Kind); err != nil {
			return err
		}
		if err = tx.Link(p, model.Relation{From: "entry:" + e.ID, Type: r.Type, To: "entry:" + other.ID}); err != nil {
			return err
		}
	}
	existing, err := tx.Relations("entry:" + e.ID)
	if err != nil {
		return err
	}
	if len(existing) > 100 {
		return fmt.Errorf("%w: at most 100 outgoing relations per entry", model.ErrInvalidInput)
	}
	return nil
}

func currentActor() string {
	if a := os.Getenv("ELEPHANT_ACTOR_ID"); a != "" {
		return a
	}
	return "unknown"
}
