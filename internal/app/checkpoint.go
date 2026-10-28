package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	gitrepo "elephant/internal/git"
	"elephant/internal/model"
	"elephant/internal/storage"
)

// CheckpointInput creates one session boundary. Completed work, next
// actions, commands, and failures are ordered lists with bounded sizes.
// RelatedFiles are repository-relative paths; Relations link to existing
// entries in the same project. StartCommit defaults to the previous
// checkpoint's end commit when present, otherwise to the current HEAD; an
// explicitly supplied start commit is verified with Git and rejected when
// unknown.
type CheckpointInput struct {
	Summary      string          `json:"summary"`
	Completed    []string        `json:"completed,omitempty"`
	Next         []string        `json:"next,omitempty"`
	Commands     []string        `json:"commands,omitempty"`
	Failures     []string        `json:"failures,omitempty"`
	StartCommit  string          `json:"start_commit,omitempty"`
	RelatedFiles []string        `json:"related_files,omitempty"`
	Relations    []RelationInput `json:"relations,omitempty"`
}

// CheckpointRecord pairs a checkpoint with its outgoing graph relationships.
type CheckpointRecord struct {
	Checkpoint model.Checkpoint `json:"checkpoint"`
	Relations  []model.Relation `json:"relations"`
}

func (s *Service) AddCheckpoint(ctx context.Context, cwd string, in CheckpointInput) (out CheckpointRecord, err error) {
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error {
		if len(in.RelatedFiles)+len(in.Relations) > 99 {
			return fmt.Errorf("%w: at most 99 related files and entries per checkpoint", model.ErrInvalidInput)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		start := in.StartCommit
		if start == "" {
			previous, err := tx.ListCheckpoints(p, 1, 0)
			if err != nil {
				return err
			}
			if len(previous) == 1 && previous[0].EndCommit != nil {
				start = *previous[0].EndCommit
			} else {
				start = g.Head
			}
		} else if err = gitrepo.VerifyCommit(ctx, g.Root, start); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, gitrepo.ErrUnknownCommit) {
				return fmt.Errorf("%w: unknown start commit %q", model.ErrInvalidInput, start)
			}
			return err
		}
		c := model.Checkpoint{
			ID: id.String(), ActorID: currentActor(), Summary: in.Summary,
			Completed: in.Completed, Next: in.Next, Commands: in.Commands, Failures: in.Failures,
			StartCommit: optional(start), EndCommit: optional(g.Head),
			CreatedAt: now, UpdatedAt: now,
		}
		if err = c.Validate(); err != nil {
			return err
		}
		if err = tx.PutCheckpoint(p, c); err != nil {
			return err
		}
		node := model.CheckpointNode(p.TableName, c.ID)
		for _, f := range in.RelatedFiles {
			clean, err := repoFile(g, f)
			if err != nil {
				return err
			}
			if err = tx.Link(p, model.Relation{From: node, Type: "concerns", To: "file:" + p.ID + ":" + clean}); err != nil {
				return err
			}
		}
		for _, r := range in.Relations {
			if r.EntryID == c.ID {
				return fmt.Errorf("%w: cannot link a checkpoint to itself", model.ErrInvalidInput)
			}
			other, err := tx.Get(p, r.EntryID)
			if err != nil {
				return err
			}
			switch r.Type {
			case "supports", "implements", "relates", "continues":
			default:
				// Accept the entry vocabulary as well so agents can reuse
				// familiar edge types when pointing at existing work.
				if err = model.ValidateRelation(other.Kind, r.Type, other.Kind); err != nil {
					// Fall through to explicit allow-list check below.
					if r.Type != "supersedes" && r.Type != "depends_on" && r.Type != "affects" && r.Type != "concerns" && r.Type != "modifies" {
						return fmt.Errorf("%w: unsupported checkpoint relation %q", model.ErrInvalidInput, r.Type)
					}
				}
			}
			_ = other
			if err = tx.Link(p, model.Relation{From: node, Type: r.Type, To: model.EntryNode(p, r.EntryID)}); err != nil {
				return err
			}
		}
		links, err := tx.Relations(node)
		if err != nil {
			return err
		}
		if len(links) > 100 {
			return fmt.Errorf("%w: at most 100 outgoing relations per checkpoint", model.ErrInvalidInput)
		}
		out = CheckpointRecord{Checkpoint: c, Relations: links}
		return nil
	})
	return
}

func (s *Service) ListCheckpoints(ctx context.Context, cwd string, limit, offset int) (out []model.Checkpoint, err error) {
	if limit < 0 || limit > 200 || offset < 0 {
		return nil, fmt.Errorf("%w: limit must be 0–200 and offset nonnegative", model.ErrInvalidInput)
	}
	if limit == 0 {
		limit = 10
	}
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, _ gitrepo.Metadata) error {
		var err error
		out, err = tx.ListCheckpoints(p, limit, offset)
		return err
	})
	return
}
