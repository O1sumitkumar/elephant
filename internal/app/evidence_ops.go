package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	gitrepo "elephant/internal/git"
	"elephant/internal/model"
	"elephant/internal/storage"
)

func (s *Service) evidenceReport(ctx context.Context, cwd, entryID string) (out EvidenceReport, err error) {
	out = newEvidenceReport(entryID)
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error {
		if _, err := tx.Get(p, entryID); err != nil {
			return err
		}
		stored, err := tx.Evidence(p, entryID)
		if err != nil {
			return err
		}
		for _, evidence := range stored {
			view, err := verifyEvidence(ctx, g.Root, evidence)
			if err != nil {
				return err
			}
			out.add(view)
		}
		return nil
	})
	return
}

// ListEvidence reports stored evidence and its current state. It is read-only.
func (s *Service) ListEvidence(ctx context.Context, cwd, entryID string) (out []EvidenceView, err error) {
	report, err := s.VerifyEvidence(ctx, cwd, entryID)
	return report.Evidence, err
}

// VerifyEvidence reports every recorded location for an entry without changing
// stored evidence or entry lifecycle state.
func (s *Service) VerifyEvidence(ctx context.Context, cwd, entryID string) (EvidenceReport, error) {
	return s.evidenceReport(ctx, cwd, entryID)
}

// AddEvidence records that a fact's knowledge came from a repository-relative
// file, capturing the current HEAD commit and the stable digest of the
// selected line (or whole file) as read from that commit. Dirty working-tree
// bytes are never recorded.
func (s *Service) AddEvidence(ctx context.Context, cwd, entryID, path string, line int) (out EvidenceView, err error) {
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error {
		e, err := tx.Get(p, entryID)
		if err != nil {
			return err
		}
		if e.Kind != model.Fact {
			return fmt.Errorf("%w: evidence can only be attached to facts", model.ErrInvalidInput)
		}
		clean, err := repoFile(g, path)
		if err != nil {
			return err
		}
		if line, err = model.CleanEvidenceLine(line); err != nil {
			return err
		}
		existing, err := tx.Evidence(p, entryID)
		if err != nil {
			return err
		}
		if len(existing) >= MaxEvidencePerEntry {
			return fmt.Errorf("%w: at most %d evidence locations per entry", model.ErrInvalidInput, MaxEvidencePerEntry)
		}
		blob, err := captureEvidenceDigest(ctx, g.Root, g.Head, clean, line)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		evidence := model.Evidence{ID: uuid.Must(uuid.NewV7()).String(), EntryID: entryID, Path: clean, Line: line, Commit: g.Head, Blob: blob, CreatedAt: now, VerifiedAt: now}
		if err = tx.PutEvidence(p, evidence); err != nil {
			return err
		}
		out, err = verifyEvidence(ctx, g.Root, evidence)
		return err
	})
	return
}

// RefreshEvidence re-captures the commit digest and verification time for one
// evidence row, or for every row of an entry when evidenceID is empty. The
// digest is read from the current HEAD, never from dirty working-tree bytes.
// The entry's lifecycle state is untouched.
func (s *Service) RefreshEvidence(ctx context.Context, cwd, entryID, evidenceID string) (out EvidenceReport, err error) {
	out = newEvidenceReport(entryID)
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error {
		if _, err := tx.Get(p, entryID); err != nil {
			return err
		}
		stored, err := tx.Evidence(p, entryID)
		if err != nil {
			return err
		}
		matched := false
		for _, evidence := range stored {
			if evidenceID != "" && evidence.ID != evidenceID {
				continue
			}
			matched = true
			blob, err := captureEvidenceDigest(ctx, g.Root, g.Head, evidence.Path, evidence.Line)
			if err != nil {
				return err
			}
			evidence.Commit = g.Head
			evidence.Blob = blob
			evidence.VerifiedAt = time.Now().UTC()
			if err = tx.PutEvidence(p, evidence); err != nil {
				return err
			}
			view, err := verifyEvidence(ctx, g.Root, evidence)
			if err != nil {
				return err
			}
			out.add(view)
		}
		if evidenceID != "" && !matched {
			return model.ErrEvidenceNotFound
		}
		return nil
	})
	return
}

// RemoveEvidence deletes one stored evidence location.
func (s *Service) RemoveEvidence(ctx context.Context, cwd, evidenceID string) (out map[string]string, err error) {
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error {
		if err := tx.DeleteEvidence(p, evidenceID); err != nil {
			return err
		}
		out = map[string]string{"removed": evidenceID}
		return nil
	})
	return
}
