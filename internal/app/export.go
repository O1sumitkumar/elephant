package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	gitrepo "elephant/internal/git"
	"elephant/internal/model"
	"elephant/internal/storage"
)

// ExportFormatVersion is the only understood envelope version. Newer versions
// fail explicitly rather than importing partially. Version 2 covers the
// complete project state: entries, relations, evidence, checkpoints, and
// adoption provenance with their metadata. Version 1 envelopes (entries and
// relations only) remain readable on import.
const ExportFormatVersion = 2

// Export limits bound archive size before any work begins.
const (
	maxExportEntries     = 10000
	maxExportRelations   = 20000
	maxExportEvidence    = 500000
	maxExportCheckpoints = 10000
	maxExportAdoptions   = 10000
	maxExportBytes       = 32 << 20
)

// ExportProject identifies the exported project and the installation that
// exported it. The receiver records the sender as provenance but always
// places rows in its own source table, so imports never overwrite local
// truth and never trust remote table ownership.
type ExportProject struct {
	Identity         string `json:"identity"`
	Name             string `json:"name"`
	SourceElephantID string `json:"source_elephant_id"`
}

// ExportEnvelope is the portable, inspectable backup of one project's
// memory: format version, project and source metadata, entries sorted by ID,
// relations sorted by from/type/to, evidence sorted by entry/creation/ID,
// checkpoints in creation order, and adoption receipts sorted by source and
// source entry. The envelope carries no wall-clock fields, so identical
// stored state serializes byte-identically.
type ExportEnvelope struct {
	FormatVersion int                `json:"format_version"`
	Project       ExportProject      `json:"project"`
	Entries       []model.Entry      `json:"entries"`
	Relations     []model.Relation   `json:"relations"`
	Evidence      []model.Evidence   `json:"evidence"`
	Checkpoints   []model.Checkpoint `json:"checkpoints"`
	Adoptions     []model.Adoption   `json:"adoptions"`
}

// Export returns a deterministic snapshot of the local project. Actors,
// lifecycle fields (status, target version, start and end commits), and
// timestamps ride inside the records themselves.
func (s *Service) Export(ctx context.Context, cwd string) (out ExportEnvelope, err error) {
	g, err := gitrepo.Inspect(ctx, cwd)
	if err != nil {
		return out, err
	}
	out.FormatVersion = ExportFormatVersion
	out.Project = ExportProject{Identity: g.Identity, Name: g.Name}
	out.Entries = []model.Entry{}
	out.Relations = []model.Relation{}
	out.Evidence = []model.Evidence{}
	out.Checkpoints = []model.Checkpoint{}
	out.Adoptions = []model.Adoption{}
	err = s.store.Transact(ctx, func(tx storage.Tx) error {
		local, err := resolve(tx, g)
		if err != nil {
			return err
		}
		self, err := tx.Identity()
		if err != nil {
			return err
		}
		out.Project.SourceElephantID = self
		offset := 0
		for {
			page, err := tx.List(local, model.Filter{Limit: 200, Offset: offset})
			if err != nil {
				return err
			}
			if len(page) == 0 {
				break
			}
			out.Entries = append(out.Entries, page...)
			offset += len(page)
			if len(page) < 200 {
				break
			}
		}
		seen := map[string]bool{}
		for _, e := range out.Entries {
			links, err := tx.Relations(model.EntryNode(local, e.ID))
			if err != nil {
				return err
			}
			for _, r := range links {
				key := r.From + "\x00" + r.Type + "\x00" + r.To
				if !seen[key] {
					seen[key] = true
					out.Relations = append(out.Relations, r)
				}
			}
			stored, err := tx.Evidence(local, e.ID)
			if err != nil {
				return err
			}
			out.Evidence = append(out.Evidence, stored...)
		}
		for offset := 0; ; offset += 200 {
			page, err := tx.ListCheckpoints(local, 200, offset)
			if err != nil {
				return err
			}
			if len(page) == 0 {
				break
			}
			out.Checkpoints = append(out.Checkpoints, page...)
			if len(page) < 200 {
				break
			}
		}
		sources, err := tx.Sources()
		if err != nil {
			return err
		}
		// Sources() spans every project in the database. Only this exported
		// project's source tables drive receipt collection, and each source
		// Elephant is queried once: the same Elephant can exist as a source
		// table under several project identities, and re-querying would
		// duplicate this project's receipts in the envelope.
		seenSources := map[string]bool{}
		for _, src := range sources {
			if src.Identity != local.Identity {
				continue
			}
			if seenSources[src.SourceElephantID] {
				continue
			}
			seenSources[src.SourceElephantID] = true
			receipts, err := tx.AdoptionsBySource(g.Identity, src.SourceElephantID)
			if err != nil {
				return err
			}
			out.Adoptions = append(out.Adoptions, receipts...)
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	sortExportEnvelope(&out)
	return out, nil
}

func sortExportEnvelope(env *ExportEnvelope) {
	sort.Slice(env.Entries, func(i, j int) bool { return env.Entries[i].ID < env.Entries[j].ID })
	sort.Slice(env.Relations, func(i, j int) bool {
		a, b := env.Relations[i], env.Relations[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.To < b.To
	})
	sort.Slice(env.Evidence, func(i, j int) bool {
		a, b := env.Evidence[i], env.Evidence[j]
		if a.EntryID != b.EntryID {
			return a.EntryID < b.EntryID
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	// Checkpoints are UUIDv7 IDs, so ID order is creation order and stable
	// across exports.
	sort.Slice(env.Checkpoints, func(i, j int) bool { return env.Checkpoints[i].ID < env.Checkpoints[j].ID })
	sort.Slice(env.Adoptions, func(i, j int) bool {
		a, b := env.Adoptions[i], env.Adoptions[j]
		if a.SourceElephantID != b.SourceElephantID {
			return a.SourceElephantID < b.SourceElephantID
		}
		return a.SourceEntryID < b.SourceEntryID
	})
}

// exportContentDigest is the canonical digest of validated archive content.
// Every record section marshals deterministically after sorting, so the
// digest keys import idempotency: same archive plus same digest is a repeat,
// same archive plus a different digest is an explicit conflict.
func exportContentDigest(env ExportEnvelope) (string, error) {
	payload, err := json.Marshal(struct {
		Project     ExportProject      `json:"project"`
		Entries     []model.Entry      `json:"entries"`
		Relations   []model.Relation   `json:"relations"`
		Evidence    []model.Evidence   `json:"evidence"`
		Checkpoints []model.Checkpoint `json:"checkpoints"`
		Adoptions   []model.Adoption   `json:"adoptions"`
	}{env.Project, env.Entries, env.Relations, env.Evidence, env.Checkpoints, env.Adoptions})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum), nil
}

// decodeExportEnvelope strictly decodes an archive: unknown fields and
// trailing data are rejected before anything is validated or stored. Version
// 1 archives (entries and relations only) are accepted and upgraded in
// memory; version 2 must carry every section.
func decodeExportEnvelope(data []byte) (ExportEnvelope, error) {
	var env ExportEnvelope
	if len(data) > maxExportBytes {
		return env, fmt.Errorf("%w: import file too large", model.ErrInvalidInput)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&env); err != nil {
		return env, fmt.Errorf("%w: invalid export JSON: %v", model.ErrInvalidInput, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return env, fmt.Errorf("%w: trailing data after export envelope", model.ErrInvalidInput)
	}
	if env.FormatVersion == 1 {
		env.Evidence = []model.Evidence{}
		env.Checkpoints = []model.Checkpoint{}
		env.Adoptions = []model.Adoption{}
	}
	return env, nil
}

// Import validates an archive completely before committing anything, then
// stores it in a separate archive source table so local truth is never
// overwritten. Idempotency is keyed by archive identity plus the canonical
// content digest: repeating an identical import returns the existing source,
// while conflicting content under the same archive name fails explicitly.
// A failed import rolls back tables, rows, graph nodes and edges,
// checkpoints, evidence, and receipts together.
func (s *Service) Import(ctx context.Context, data []byte, asRemote string) (out model.Project, err error) {
	env, err := decodeExportEnvelope(data)
	if err != nil {
		return out, err
	}
	if env.FormatVersion != ExportFormatVersion && env.FormatVersion != 1 {
		return out, fmt.Errorf("%w: unsupported export format version %d (expected %d)", model.ErrInvalidInput, env.FormatVersion, ExportFormatVersion)
	}
	if asRemote == "" {
		asRemote = "archive"
	}
	if !validText(asRemote, 100) || validID(asRemote) {
		return out, fmt.Errorf("%w: archive name required (not a UUID)", model.ErrInvalidInput)
	}
	if !validText(env.Project.Identity, 2048) || !validText(env.Project.Name, 300) || !validText(env.Project.SourceElephantID, 300) {
		return out, fmt.Errorf("%w: export project and source identity required", model.ErrInvalidInput)
	}
	if len(env.Entries) > maxExportEntries || len(env.Relations) > maxExportRelations || len(env.Evidence) > maxExportEvidence || len(env.Checkpoints) > maxExportCheckpoints || len(env.Adoptions) > maxExportAdoptions {
		return out, fmt.Errorf("%w: export too large", model.ErrInvalidInput)
	}
	if env.Entries == nil || env.Relations == nil {
		return out, fmt.Errorf("%w: export entries and relations are required", model.ErrInvalidInput)
	}
	// Full validation before any mutation.
	ids := map[string]model.Entry{}
	for _, e := range env.Entries {
		if !validID(e.ID) {
			return out, fmt.Errorf("%w: entry ID must be UUIDv7", model.ErrInvalidInput)
		}
		if _, dup := ids[e.ID]; dup {
			return out, fmt.Errorf("%w: duplicate entry ID", model.ErrInvalidInput)
		}
		if err := e.Validate(); err != nil {
			return out, err
		}
		if e.CreatedAt.IsZero() || e.UpdatedAt.Before(e.CreatedAt) {
			return out, fmt.Errorf("%w: entry timestamps required", model.ErrInvalidInput)
		}
		if !validText(e.ActorID, 300) {
			return out, fmt.Errorf("%w: actor ID required", model.ErrInvalidInput)
		}
		ids[e.ID] = e
	}
	for _, r := range env.Relations {
		fromID, ok := entryIDFromNode(r.From)
		if !ok {
			return out, fmt.Errorf("%w: invalid relation source", model.ErrInvalidInput)
		}
		from, ok := ids[fromID]
		if !ok {
			return out, fmt.Errorf("%w: relation source unknown", model.ErrInvalidInput)
		}
		if isFileNode(r.To) {
			file := filePathFromNode(r.To)
			if _, err := model.CleanFile(file); err != nil {
				return out, err
			}
			if r.Type != model.FileEdge(from.Kind) {
				return out, fmt.Errorf("%w: invalid file edge", model.ErrInvalidInput)
			}
			continue
		}
		toID, ok := entryIDFromNode(r.To)
		if !ok {
			return out, fmt.Errorf("%w: invalid relation target", model.ErrInvalidInput)
		}
		to, ok := ids[toID]
		if !ok {
			return out, fmt.Errorf("%w: relation target unknown (ownership crossings rejected)", model.ErrInvalidInput)
		}
		if err := model.ValidateRelation(from.Kind, r.Type, to.Kind); err != nil {
			return out, err
		}
	}
	evidenceByEntry := map[string][]model.Evidence{}
	evidenceIDs := map[string]bool{}
	for _, ev := range env.Evidence {
		if !validID(ev.ID) {
			return out, fmt.Errorf("%w: evidence ID must be UUIDv7", model.ErrInvalidInput)
		}
		if _, dup := evidenceIDs[ev.ID]; dup {
			return out, fmt.Errorf("%w: duplicate evidence ID", model.ErrInvalidInput)
		}
		if err := ev.Validate(); err != nil {
			return out, err
		}
		if _, ok := ids[ev.EntryID]; !ok {
			return out, fmt.Errorf("%w: evidence targets unknown entry", model.ErrInvalidInput)
		}
		evidenceIDs[ev.ID] = true
		evidenceByEntry[ev.EntryID] = append(evidenceByEntry[ev.EntryID], ev)
	}
	for _, rows := range evidenceByEntry {
		if len(rows) > MaxEvidencePerEntry {
			return out, fmt.Errorf("%w: at most %d evidence locations per entry", model.ErrInvalidInput, MaxEvidencePerEntry)
		}
	}
	checkpointIDs := map[string]bool{}
	for _, c := range env.Checkpoints {
		if !validID(c.ID) {
			return out, fmt.Errorf("%w: checkpoint ID must be UUIDv7", model.ErrInvalidInput)
		}
		if _, dup := checkpointIDs[c.ID]; dup {
			return out, fmt.Errorf("%w: duplicate checkpoint ID", model.ErrInvalidInput)
		}
		if err := c.Validate(); err != nil {
			return out, err
		}
		if !validText(c.ActorID, 300) {
			return out, fmt.Errorf("%w: checkpoint actor ID required", model.ErrInvalidInput)
		}
		checkpointIDs[c.ID] = true
	}
	adoptIDs := map[string]bool{}
	adoptKeys := map[string]bool{}
	for _, a := range env.Adoptions {
		if !validID(a.ID) || !validID(a.SourceEntryID) || !validID(a.LocalEntryID) {
			return out, fmt.Errorf("%w: adoption IDs must be UUIDv7", model.ErrInvalidInput)
		}
		if a.ProjectIdentity != env.Project.Identity {
			return out, fmt.Errorf("%w: adoption provenance from a foreign project", model.ErrInvalidInput)
		}
		if _, ok := ids[a.LocalEntryID]; !ok {
			return out, fmt.Errorf("%w: adoption references unknown local entry", model.ErrInvalidInput)
		}
		if validID(a.SourceElephantID) && a.SourceElephantID == a.LocalEntryID {
			return out, fmt.Errorf("%w: adoption provenance is self-referential", model.ErrInvalidInput)
		}
		if adoptIDs[a.ID] {
			return out, fmt.Errorf("%w: duplicate adoption receipt ID", model.ErrInvalidInput)
		}
		// One archive cannot carry two receipts for the same source entry:
		// provenance must stay unambiguous.
		key := a.SourceElephantID + "\x00" + a.SourceEntryID
		if adoptKeys[key] {
			return out, fmt.Errorf("%w: duplicate adoption receipt for source entry %s", model.ErrInvalidInput, a.SourceEntryID)
		}
		adoptIDs[a.ID] = true
		adoptKeys[key] = true
	}
	// Idempotency: an identical reimport returns the existing source; the
	// same name with conflicting content fails explicitly (Message reports a
	// digest mismatch).
	sortExportEnvelope(&env)
	contentDigest, err := exportContentDigest(env)
	if err != nil {
		return out, err
	}
	sourceID := "import:" + asRemote
	err = s.store.Transact(ctx, func(tx storage.Tx) error {
		seen, err := importSeen(tx, sourceID, env.Project.Identity, contentDigest)
		if errors.Is(err, model.ErrInvalidInput) {
			return fmt.Errorf("%w: archive %q was already imported with different content", model.ErrInvalidInput, asRemote)
		}
		if err != nil {
			return err
		}
		if seen {
			src, err := tx.Source(env.Project.Identity, sourceID)
			if err != nil {
				return err
			}
			out = src
			return nil
		}
		src, err := tx.EnsureSource(model.Project{Identity: env.Project.Identity, Name: env.Project.Name}, sourceID)
		if err != nil {
			return err
		}
		out = src
		for _, e := range env.Entries {
			if err = tx.Put(src, e); err != nil {
				return err
			}
		}
		for _, r := range env.Relations {
			fromID, _ := entryIDFromNode(r.From)
			toRewrite := r
			toRewrite.From = model.EntryNode(src, fromID)
			if !isFileNode(r.To) {
				toID, _ := entryIDFromNode(r.To)
				toRewrite.To = model.EntryNode(src, toID)
			} else {
				toRewrite.To = "file:" + src.ID + ":" + filePathFromNode(r.To)
			}
			if err = tx.Link(src, toRewrite); err != nil {
				return err
			}
		}
		for _, ev := range env.Evidence {
			if err = tx.PutEvidence(src, ev); err != nil {
				return err
			}
		}
		for _, c := range env.Checkpoints {
			if err = tx.PutCheckpoint(src, c); err != nil {
				return err
			}
		}
		// Imported receipts stay archived under this archive's table. They
		// never enter the live adoption registry, so they cannot satisfy,
		// shadow, or overwrite a local adoption decision.
		for _, a := range env.Adoptions {
			if err = tx.RecordArchivedAdoption(src.TableName, a); err != nil {
				return err
			}
		}
		return nil
	})
	return
}

// importSeen reports whether this content was already imported under sourceID
// for the project, and fails with ErrInvalidInput when different content was.
// Receipts are keyed by archive name and project identity, so projects can
// share an archive name. Archives imported before that have a receipt keyed by
// name alone, which belongs to the project whose source table already exists.
func importSeen(tx storage.Tx, sourceID, identity, digest string) (bool, error) {
	_, srcErr := tx.Source(identity, sourceID)
	if srcErr != nil && !errors.Is(srcErr, model.ErrProjectNotFound) {
		return false, srcErr
	}
	// Names and identities cannot contain newlines, so the key is unambiguous.
	seen, err := tx.Message(sourceID+"\n"+identity, digest, "import")
	if err != nil || seen || srcErr != nil {
		return seen, err
	}
	return tx.Message(sourceID, digest, "import")
}

func entryIDFromNode(n string) (string, bool) {
	after, ok := strings.CutPrefix(n, "entry:")
	if !ok {
		return "", false
	}
	// Local form entry:<uuid>; remote form entry:<table>:<uuid>.
	if i := strings.LastIndex(after, ":"); i >= 0 {
		after = after[i+1:]
	}
	return after, validID(after)
}

func isFileNode(n string) bool { return strings.HasPrefix(n, "file:") }

func filePathFromNode(n string) string {
	rest := strings.TrimPrefix(n, "file:")
	// Exported file nodes are file:<projectID>:<path>; be tolerant of legacy forms.
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) == 3 {
		return parts[2]
	}
	if i := strings.Index(rest, ":"); i >= 0 {
		return rest[i+1:]
	}
	return rest
}

// Backup writes a storage-safe consistent snapshot to dest while the database
// stays open. The snapshot is staged in a sibling temporary file and renamed
// into place only on success, so a failed backup preserves any existing
// destination and leaves no litter behind. The rename replaces existing
// destinations on Unix and on Windows: os.Rename maps to MoveFileEx with
// MOVEFILE_REPLACE_EXISTING, and a lingering read handle from a concurrent
// reader is resolved by one bounded replace window before failing.
func (s *Service) Backup(ctx context.Context, dest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if filepath.Ext(dest) != ".zova" {
		return fmt.Errorf("%w: backup destination must end in .zova", model.ErrInvalidInput)
	}
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".elephant-backup-*.zova")
	if err != nil {
		return fmt.Errorf("%w: stage backup: %v", model.ErrInvalidInput, err)
	}
	tmpName := tmp.Name()
	if err = tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("%w: stage backup: %v", model.ErrInvalidInput, err)
	}
	// Zova requires a .zova destination and refuses any existing file, so the
	// reserved placeholder is removed before the store writes the snapshot.
	if err = os.Remove(tmpName); err != nil {
		return fmt.Errorf("%w: stage backup: %v", model.ErrInvalidInput, err)
	}
	if err = s.store.Backup(tmpName); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err = replaceFile(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// replaceFile moves src onto dest, replacing any existing destination. On
// Unix os.Rename already does this. On Windows os.Rename maps to MoveFileEx
// with MOVEFILE_REPLACE_EXISTING, which fails while another handle still has
// the destination open with sharing restrictions; a short retry window lets a
// concurrent reader finish so repeated backups can safely replace the
// destination on Windows as well as Unix.
func replaceFile(src, dest string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(src, dest)
	}
	const attempts = 10
	const delay = 100 * time.Millisecond
	var err error
	for i := 0; i < attempts; i++ {
		if err = os.Rename(src, dest); err == nil {
			return nil
		}
		var pathErr *os.LinkError
		if !errors.As(err, &pathErr) {
			break
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("%w: %w", model.ErrStorage, err)
}
