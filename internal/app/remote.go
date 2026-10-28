package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	gitrepo "elephant/internal/git"
	"elephant/internal/model"
	"elephant/internal/storage"
)

type ProjectRef struct {
	Identity string `json:"identity"`
	Name     string `json:"name"`
}
type Message struct {
	ProtocolVersion  int              `json:"protocol_version"`
	MessageID        string           `json:"message_id"`
	SenderElephantID string           `json:"sender_elephant_id"`
	Project          ProjectRef       `json:"project"`
	Operation        string           `json:"operation"`
	Entry            *model.Entry     `json:"entry,omitempty"`
	Files            []string         `json:"files,omitempty"`
	Relations        []RelationInput  `json:"relations,omitempty"`
	Evidence         []model.Evidence `json:"evidence,omitempty"`
}
type Response struct {
	EntryID         string        `json:"entry_id,omitempty"`
	ProtocolVersion int           `json:"protocol_version"`
	MessageID       string        `json:"message_id"`
	ElephantID      string        `json:"elephant_id"`
	Table           model.Project `json:"table"`
	Recall          *RecallResult `json:"recall,omitempty"`
	Error           string        `json:"error,omitempty"`
}

func validID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.Version() == 7 && u.String() == s
}
func validText(s string, n int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= n && !strings.ContainsAny(s, "\x00\r\n")
}
func validateMessage(m Message) error {
	if m.ProtocolVersion != 1 || !validID(m.MessageID) || !validID(m.SenderElephantID) {
		return fmt.Errorf("%w: protocol version 1 and UUIDv7 IDs required", model.ErrInvalidInput)
	}
	if !validText(m.Project.Identity, 2048) || !validText(m.Project.Name, 300) || strings.HasPrefix(m.Project.Identity, "local:") || strings.HasPrefix(m.Project.Identity, "/") {
		return fmt.Errorf("%w: portable project identity required", model.ErrInvalidInput)
	}
	normalized, err := gitrepo.NormalizeRemote("https://" + m.Project.Identity)
	if err != nil || normalized != m.Project.Identity || strings.ContainsAny(m.Project.Identity, "\\ @\t") || strings.Contains("/"+m.Project.Identity+"/", "/../") {
		return fmt.Errorf("%w: canonical host/repository project identity required", model.ErrInvalidInput)
	}
	if m.Operation != "entry.send" && m.Operation != "project.ensure" && m.Operation != "project.recall" {
		return fmt.Errorf("%w: unknown operation", model.ErrInvalidInput)
	}
	if m.Operation != "entry.send" {
		if m.Entry != nil || len(m.Files)+len(m.Relations)+len(m.Evidence) > 0 {
			return model.ErrInvalidInput
		}
		return nil
	}
	if m.Entry == nil || !validID(m.Entry.ID) || !validText(m.Entry.ActorID, 300) || m.Entry.CreatedAt.IsZero() || m.Entry.UpdatedAt.Before(m.Entry.CreatedAt) {
		return fmt.Errorf("%w: entry ID, actor and timestamps required", model.ErrInvalidInput)
	}
	if err := m.Entry.Validate(); err != nil {
		return err
	}
	for _, c := range []*string{m.Entry.StartCommit, m.Entry.EndCommit} {
		if c != nil && (!validText(*c, 128) || strings.ContainsAny(*c, " \t")) {
			return model.ErrInvalidInput
		}
	}
	if len(m.Files)+len(m.Relations) > 99 {
		return model.ErrInvalidInput
	}
	for _, f := range m.Files {
		if strings.Contains("/"+f+"/", "/../") {
			return fmt.Errorf("%w: path traversal is not allowed", model.ErrInvalidInput)
		}
		if _, err := model.CleanFile(f); err != nil {
			return err
		}
	}
	for _, r := range m.Relations {
		if r.Type != "supersedes" && r.Type != "depends_on" && r.Type != "implements" && r.Type != "supports" {
			return model.ErrInvalidInput
		}
		if !validID(r.EntryID) || r.EntryID == m.Entry.ID {
			return model.ErrInvalidInput
		}
	}
	if len(m.Evidence) > MaxEvidencePerEntry {
		return fmt.Errorf("%w: at most %d evidence locations per entry", model.ErrInvalidInput, MaxEvidencePerEntry)
	}
	if len(m.Evidence) > 0 && m.Entry.Kind != model.Fact {
		return fmt.Errorf("%w: evidence can only be attached to facts", model.ErrInvalidInput)
	}
	seen := map[string]bool{}
	for _, evidence := range m.Evidence {
		if !validID(evidence.ID) || evidence.EntryID != m.Entry.ID {
			return fmt.Errorf("%w: evidence IDs must be UUIDv7 and target the sent entry", model.ErrInvalidInput)
		}
		if seen[evidence.ID] {
			return fmt.Errorf("%w: duplicate evidence ID", model.ErrInvalidInput)
		}
		seen[evidence.ID] = true
		if err := evidence.Validate(); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) Identity(ctx context.Context) (id string, err error) {
	err = s.store.Transact(ctx, func(tx storage.Tx) error { id, err = tx.Identity(); return err })
	return
}
func digest(v any) string { b, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(b)) }
func (s *Service) Receive(ctx context.Context, m Message) (out Response, err error) {
	out.ProtocolVersion = 1
	out.MessageID = m.MessageID
	if err = validateMessage(m); err != nil {
		return out, err
	}
	err = s.store.Transact(ctx, func(tx storage.Tx) error {
		var err error
		out.ElephantID, err = tx.Identity()
		if err != nil {
			return err
		}
		if out.ElephantID == m.SenderElephantID {
			return fmt.Errorf("%w: cannot receive own state", model.ErrInvalidInput)
		}
		if m.Operation == "project.recall" {
			out.Table, err = tx.Source(m.Project.Identity, m.SenderElephantID)
			return err
		}
		out.Table, err = tx.EnsureSource(model.Project{Identity: m.Project.Identity, Name: m.Project.Name}, m.SenderElephantID)
		if err != nil {
			return err
		}
		if m.Entry != nil {
			out.EntryID = m.Entry.ID
		}
		if _, err = tx.Message(m.MessageID, digest(m), "message"); err != nil {
			return err
		}
		if m.Operation == "project.ensure" {
			return nil
		}
		entryDigest := digest(struct {
			Entry     *model.Entry
			Files     []string
			Relations []RelationInput
			Evidence  []model.Evidence
		}{m.Entry, m.Files, m.Relations, m.Evidence})
		seen, err := tx.Message(m.Entry.ID, entryDigest, "entry:"+out.Table.TableName)
		if err != nil || seen {
			return err
		}
		p := out.Table
		e := *m.Entry
		if err = tx.Put(p, e); err != nil {
			return err
		}
		for _, f := range m.Files {
			clean, _ := model.CleanFile(f)
			if err = tx.Link(p, model.Relation{From: model.EntryNode(p, e.ID), Type: model.FileEdge(e.Kind), To: "file:" + p.ID + ":" + clean}); err != nil {
				return err
			}
		}
		for _, r := range m.Relations {
			other, err := tx.Get(p, r.EntryID)
			if err != nil {
				return err
			}
			if err = model.ValidateRelation(e.Kind, r.Type, other.Kind); err != nil {
				return err
			}
			if err = tx.Link(p, model.Relation{From: model.EntryNode(p, e.ID), Type: r.Type, To: model.EntryNode(p, r.EntryID)}); err != nil {
				return err
			}
		}
		for _, evidence := range m.Evidence {
			evidence.EntryID = e.ID
			if err := tx.PutEvidence(p, evidence); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && m.Operation == "project.recall" {
		r, e := s.RecallSource(ctx, m.Project, m.SenderElephantID, "")
		out.Recall = &r
		err = e
	}
	return
}
func (s *Service) RecallSource(ctx context.Context, ref ProjectRef, source, version string) (RecallResult, error) {
	return s.recallWith(ctx, version, func(fn func(storage.Tx, model.Project, gitrepo.Metadata) error) error {
		return s.store.Transact(ctx, func(tx storage.Tx) error {
			p, err := tx.Source(ref.Identity, source)
			if err != nil {
				return err
			}
			if p.Scope != "remote" {
				return model.ErrInvalidInput
			}
			return fn(tx, p, gitrepo.Metadata{})
		})
	})
}
func (s *Service) RecallRemote(ctx context.Context, cwd, selector string) (RecallResult, error) {
	g, err := gitrepo.Inspect(ctx, cwd)
	if err != nil {
		return RecallResult{}, err
	}
	source := selector
	if !validID(selector) {
		r, err := s.Remote(ctx, selector)
		if err != nil {
			return RecallResult{}, err
		}
		source = r.ElephantID
		if source == "" {
			return RecallResult{}, fmt.Errorf("%w: remote identity unknown; ensure-project first or use source UUID", model.ErrInvalidInput)
		}
	}
	return s.RecallSource(ctx, ProjectRef{g.Identity, g.Name}, source, "")
}
func (s *Service) ListRemotes(ctx context.Context) (out []model.Remote, err error) {
	err = s.store.Transact(ctx, func(tx storage.Tx) error { out, err = tx.Remotes(); return err })
	return
}
func (s *Service) Remote(ctx context.Context, name string) (model.Remote, error) {
	rs, err := s.ListRemotes(ctx)
	if err != nil {
		return model.Remote{}, err
	}
	for _, r := range rs {
		if r.Name == name {
			return r, nil
		}
	}
	return model.Remote{}, fmt.Errorf("%w: unknown remote %q", model.ErrInvalidInput, name)
}
func (s *Service) RemoveRemote(ctx context.Context, name string) error {
	if _, err := s.Remote(ctx, name); err != nil {
		return err
	}
	return s.store.Transact(ctx, func(tx storage.Tx) error { return tx.RemoveRemote(name) })
}
func (s *Service) BuildMessage(ctx context.Context, cwd, operation string, k model.Kind, in CreateInput) (Message, error) {
	g, err := gitrepo.Inspect(ctx, cwd)
	if err != nil {
		return Message{}, err
	}
	self, err := s.Identity(ctx)
	if err != nil {
		return Message{}, err
	}
	m := Message{ProtocolVersion: 1, MessageID: uuid.Must(uuid.NewV7()).String(), SenderElephantID: self, Project: ProjectRef{g.Identity, g.Name}, Operation: operation}
	if operation == "entry.send" {
		now := time.Now().UTC()
		m.Entry = &model.Entry{ID: uuid.Must(uuid.NewV7()).String(), ActorID: currentActor(), Kind: k, Title: in.Title, Body: in.Body, Status: model.DefaultStatus(k), TargetVersion: in.TargetVersion, StartCommit: optional(g.Head), CreatedAt: now, UpdatedAt: now}
		if m.Files, err = repoFiles(g, in.RelatedFiles); err != nil {
			return Message{}, err
		}
		m.Relations = in.Relations
		if in.Supersedes != "" {
			m.Relations = append(m.Relations, RelationInput{Type: "supersedes", EntryID: in.Supersedes})
		}
		if in.Evidence != nil {
			attached, err := s.evidenceForSend(ctx, cwd, m.Entry.ID, in.Evidence)
			if err != nil {
				return Message{}, err
			}
			m.Evidence = attached
		}
	}
	return m, validateMessage(m)
}

// evidenceForSend resolves locally stored evidence to full records for a
// remote send. Local row IDs are never trusted across the wire boundary;
// after validation the rows are rebound to the new entry ID, so evidence
// always travels with and targets the entry it describes.
func (s *Service) evidenceForSend(ctx context.Context, cwd, entryID string, rows []EvidenceInput) (out []model.Evidence, err error) {
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, _ gitrepo.Metadata) error {
		if len(rows) > MaxEvidencePerEntry {
			return fmt.Errorf("%w: at most %d evidence locations per entry", model.ErrInvalidInput, MaxEvidencePerEntry)
		}
		for _, row := range rows {
			source, sourceErr := tx.EvidenceByID(p, row.EntryID)
			if sourceErr != nil {
				return sourceErr
			}
			e, getErr := tx.Get(p, source.EntryID)
			if getErr != nil {
				return getErr
			}
			if e.Kind != model.Fact {
				return fmt.Errorf("%w: evidence can only be attached to facts", model.ErrInvalidInput)
			}
			source.ID = uuid.Must(uuid.NewV7()).String()
			source.EntryID = entryID
			source.VerifiedAt = time.Now().UTC()
			out = append(out, source)
		}
		return nil
	})
	return
}
