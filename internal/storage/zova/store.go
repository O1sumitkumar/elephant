// Package zova implements Elephant persistence with Zova SQL and its named graph.
package zova

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	native "github.com/ata-sesli/zova/bindings/go"

	"elephant/internal/model"
	"elephant/internal/storage"
)

const graph = "elephant"

// busyTimeoutMS lets concurrent Elephant processes wait for each other's
// locks, including while opening, instead of failing immediately.
const busyTimeoutMS = 5000

type Store struct {
	mu sync.Mutex
	db *native.DB
}
type transaction struct{ db *native.DB }

func Open(path string) (*Store, error) {
	if filepath.Ext(path) != ".zova" {
		return nil, fmt.Errorf("%w: database path must end in .zova", model.ErrInvalidInput)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err = create(path); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err := migrateFormat(path); err != nil {
		return nil, err
	}
	db, err := native.OpenWithOptions(path, native.OpenOptions{BusyTimeoutMS: busyTimeoutMS})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	s := &Store{db: db}
	err = db.SetBusyTimeout(busyTimeoutMS)
	if err == nil {
		err = s.Transact(context.Background(), func(tx storage.Tx) error {
			t := tx.(*transaction)
			rows, err := t.query("SELECT value FROM elephant_meta WHERE key='schema_version'")
			if err != nil || len(rows) != 1 || (value(rows[0][0]) != "1" && value(rows[0][0]) != "2" && value(rows[0][0]) != "3" && value(rows[0][0]) != "4" && value(rows[0][0]) != "5") {
				return model.ErrSchema
			}
			has, err := db.HasGraph(graph)
			if err != nil {
				return err
			}
			if !has {
				return model.ErrSchema
			}
			switch value(rows[0][0]) {
			case "1":
				if err := t.migrate(); err != nil {
					return err
				}
				return t.migrateCheckpointKeys()
			case "2":
				if err := t.migrateEvidence(); err != nil {
					return err
				}
				if err := t.migrateCheckpoints(); err != nil {
					return err
				}
				return t.migrateCheckpointKeys()
			case "3":
				if err := t.migrateCheckpoints(); err != nil {
					return err
				}
				return t.migrateCheckpointKeys()
			case "4":
				return t.migrateCheckpointKeys()
			}
			return nil
		})
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// create builds a new database under a staging name and publishes it at path
// only once its schema is committed. A crash therefore never leaves a
// schema-less file at path, and processes creating the same path at once
// all end up opening the one database that was published first.
func create(path string) error {
	staging := fmt.Sprintf("%s.creating-%s.zova", strings.TrimSuffix(path, ".zova"), newID())
	defer removeWithSidecars(staging)
	db, err := native.Create(staging)
	if err != nil {
		return fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	err = os.Chmod(staging, 0600)
	if err == nil {
		err = (&Store{db: db}).Transact(context.Background(), func(tx storage.Tx) error {
			t := tx.(*transaction)
			if err := db.Exec(`CREATE TABLE elephant_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE projects(id TEXT PRIMARY KEY,identity TEXT NOT NULL UNIQUE,name TEXT NOT NULL,remote TEXT NOT NULL,table_name TEXT NOT NULL UNIQUE,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
INSERT INTO elephant_meta VALUES('schema_version','1');`); err != nil {
				return err
			}
			if err := db.CreateGraph(graph); err != nil {
				return err
			}
			if err := t.migrate(); err != nil {
				return err
			}
			return t.migrateCheckpointKeys()
		})
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// A hard link publishes atomically and, unlike rename, never replaces a
	// database another process published first.
	err = os.Link(staging, path)
	if err == nil || errors.Is(err, os.ErrExist) {
		return nil
	}
	// Filesystems without hard links fall back to rename, which is still
	// atomic but leaves a short window in which two creators can race.
	if _, statErr := os.Stat(path); statErr == nil {
		return nil
	}
	return os.Rename(staging, path)
}

func (s *Store) Close() error { s.mu.Lock(); defer s.mu.Unlock(); return s.db.Close() }

// Backup creates a storage-safe consistent snapshot at dest while the database
// remains open.
func (s *Store) Backup(dest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.BackupTo(dest); err != nil {
		return fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	return nil
}
func (s *Store) Transact(ctx context.Context, fn func(storage.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.db.BeginImmediate(); err != nil {
		return fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	defer s.db.Rollback()
	if err := fn(&transaction{db: s.db}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.db.Commit(); err != nil {
		return fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	return nil
}
func (t *transaction) query(sql string, args ...any) ([][]*string, error) {
	stmt, err := t.db.Prepare(sql)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	defer stmt.Close()
	for i, arg := range args {
		switch v := arg.(type) {
		case string:
			err = stmt.BindText(i+1, v)
		case *string:
			if v == nil {
				err = stmt.BindNull(i + 1)
			} else {
				err = stmt.BindText(i+1, *v)
			}
		case int:
			err = stmt.BindInt64(i+1, int64(v))
		case nil:
			err = stmt.BindNull(i + 1)
		default:
			return nil, fmt.Errorf("%w: unsupported SQL binding", model.ErrStorage)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", model.ErrStorage, err)
		}
	}
	rows := [][]*string{}
	for {
		step, err := stmt.Step()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", model.ErrStorage, err)
		}
		if step == native.StepDone {
			return rows, nil
		}
		n, err := stmt.ColumnCount()
		if err != nil {
			return nil, err
		}
		row := make([]*string, n)
		for i := range n {
			v, ok, err := stmt.ColumnText(i)
			if err != nil {
				return nil, err
			}
			if ok {
				row[i] = &v
			}
		}
		rows = append(rows, row)
	}
}
func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func stamp(t time.Time) string               { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func parseTime(s *string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value(s)) }

var tablePattern = regexp.MustCompile(`^p_[0-9a-f]{32}$`)

func table(p model.Project) (string, error) {
	if !tablePattern.MatchString(p.TableName) {
		return "", model.ErrStorage
	}
	return p.TableName, nil
}
func (t *transaction) Project(identity string) (model.Project, error) {
	rows, err := t.query("SELECT id,identity,name,remote,table_name,created_at,updated_at FROM projects WHERE identity=?", identity)
	if err != nil {
		return model.Project{}, err
	}
	if len(rows) == 0 {
		return model.Project{}, model.ErrProjectNotFound
	}
	r := rows[0]
	created, err := parseTime(r[5])
	if err != nil {
		return model.Project{}, err
	}
	updated, err := parseTime(r[6])
	if err != nil {
		return model.Project{}, err
	}
	self, err := t.Identity()
	if err != nil {
		return model.Project{}, err
	}
	return model.Project{Scope: "local", SourceElephantID: self, ID: value(r[0]), Identity: value(r[1]), Name: value(r[2]), Remote: value(r[3]), TableName: value(r[4]), CreatedAt: created, UpdatedAt: updated}, nil
}
func (t *transaction) CreateProject(p model.Project) error {
	_, err := table(p)
	if err != nil {
		return err
	}
	_, err = t.query("INSERT INTO projects VALUES(?,?,?,?,?,?,?)", p.ID, p.Identity, p.Name, p.Remote, p.TableName, stamp(p.CreatedAt), stamp(p.UpdatedAt))
	if err != nil {
		return err
	}
	if err = t.createEntryTable(p); err != nil {
		return err
	}
	return t.registerLocal(p)
}
func (t *transaction) createEntryTable(p model.Project) error {
	name, err := table(p)
	if err != nil {
		return err
	}
	err = t.db.Exec(`CREATE TABLE ` + name + ` (id TEXT PRIMARY KEY,kind TEXT NOT NULL CHECK(kind IN ('fact','decision','task')),title TEXT NOT NULL,body TEXT NOT NULL,status TEXT NOT NULL,target_version TEXT,start_commit TEXT,end_commit TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,actor_id TEXT NOT NULL);
CREATE INDEX ` + name + `_kind_status ON ` + name + `(kind,status,updated_at DESC,id DESC);
CREATE INDEX ` + name + `_created ON ` + name + `(created_at);
CREATE INDEX ` + name + `_version ON ` + name + `(target_version);`)
	if err != nil {
		return fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	return t.createEvidenceTable(name)
}

// createEvidenceTable is idempotent so schema migration can add it to projects
// that predate evidence.
func (t *transaction) createEvidenceTable(name string) error {
	err := t.db.Exec(`CREATE TABLE IF NOT EXISTS ` + name + `_evidence (id TEXT PRIMARY KEY,entry_id TEXT NOT NULL,path TEXT NOT NULL,line INTEGER,commit_hash TEXT,blob TEXT,created_at TEXT NOT NULL,verified_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS ` + name + `_evidence_entry ON ` + name + `_evidence(entry_id,created_at,id);`)
	if err != nil {
		return fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	return nil
}

const columns = "id,kind,title,body,status,target_version,start_commit,end_commit,created_at,updated_at,actor_id"

const evidenceColumns = "id,entry_id,path,line,commit_hash,blob,created_at,verified_at"

func (t *transaction) evidenceTable(p model.Project) (string, error) {
	name, err := table(p)
	if err != nil {
		return "", err
	}
	return name + "_evidence", nil
}

func (t *transaction) PutEvidence(p model.Project, e model.Evidence) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if _, err := t.Get(p, e.EntryID); err != nil {
		return err
	}
	name, err := t.evidenceTable(p)
	if err != nil {
		return err
	}
	line := any(nil)
	if e.Line > 0 {
		line = e.Line
	}
	_, err = t.query("INSERT INTO "+name+"("+evidenceColumns+") VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET path=excluded.path,line=excluded.line,commit_hash=excluded.commit_hash,blob=excluded.blob,verified_at=excluded.verified_at",
		e.ID, e.EntryID, e.Path, line, e.Commit, e.Blob, stamp(e.CreatedAt), stamp(e.VerifiedAt))
	return err
}

func (t *transaction) Evidence(p model.Project, entryID string) ([]model.Evidence, error) {
	name, err := t.evidenceTable(p)
	if err != nil {
		return nil, err
	}
	rows, err := t.query("SELECT "+evidenceColumns+" FROM "+name+" WHERE entry_id=? ORDER BY created_at,id", entryID)
	if err != nil {
		return nil, err
	}
	out := make([]model.Evidence, 0, len(rows))
	for _, row := range rows {
		evidence, err := decodeEvidence(row)
		if err != nil {
			return nil, err
		}
		out = append(out, evidence)
	}
	return out, nil
}

func (t *transaction) EvidenceByID(p model.Project, id string) (model.Evidence, error) {
	name, err := t.evidenceTable(p)
	if err != nil {
		return model.Evidence{}, err
	}
	rows, err := t.query("SELECT "+evidenceColumns+" FROM "+name+" WHERE id=?", id)
	if err != nil {
		return model.Evidence{}, err
	}
	if len(rows) == 0 {
		return model.Evidence{}, model.ErrEvidenceNotFound
	}
	return decodeEvidence(rows[0])
}

func (t *transaction) DeleteEvidence(p model.Project, id string) error {
	name, err := t.evidenceTable(p)
	if err != nil {
		return err
	}
	if _, err = t.EvidenceByID(p, id); err != nil {
		return err
	}
	_, err = t.query("DELETE FROM "+name+" WHERE id=?", id)
	return err
}

func decodeEvidence(r []*string) (model.Evidence, error) {
	created, err := parseTime(r[6])
	if err != nil {
		return model.Evidence{}, err
	}
	verified, err := parseTime(r[7])
	if err != nil {
		return model.Evidence{}, err
	}
	line := 0
	if raw := value(r[3]); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &line); err != nil {
			return model.Evidence{}, fmt.Errorf("%w: invalid evidence line %q", model.ErrStorage, raw)
		}
	}
	return model.Evidence{ID: value(r[0]), EntryID: value(r[1]), Path: value(r[2]), Line: line, Commit: value(r[4]), Blob: value(r[5]), CreatedAt: created, VerifiedAt: verified}, nil
}

func decode(r []*string) (model.Entry, error) {
	c, err := parseTime(r[8])
	if err != nil {
		return model.Entry{}, err
	}
	u, err := parseTime(r[9])
	if err != nil {
		return model.Entry{}, err
	}
	return model.Entry{ID: value(r[0]), ActorID: value(r[10]), Kind: model.Kind(value(r[1])), Title: value(r[2]), Body: value(r[3]), Status: value(r[4]), TargetVersion: r[5], StartCommit: r[6], EndCommit: r[7], CreatedAt: c, UpdatedAt: u}, nil
}
func (t *transaction) Get(p model.Project, id string) (model.Entry, error) {
	name, err := table(p)
	if err != nil {
		return model.Entry{}, err
	}
	rows, err := t.query("SELECT "+columns+" FROM "+name+" WHERE id=?", id)
	if err != nil {
		return model.Entry{}, err
	}
	if len(rows) == 0 {
		return model.Entry{}, model.ErrEntryNotFound
	}
	return decode(rows[0])
}
func (t *transaction) Put(p model.Project, e model.Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	name, err := table(p)
	if err != nil {
		return err
	}
	_, err = t.query("INSERT INTO "+name+"("+columns+") VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET title=excluded.title,body=excluded.body,status=excluded.status,target_version=excluded.target_version,end_commit=excluded.end_commit,updated_at=excluded.updated_at", e.ID, string(e.Kind), e.Title, e.Body, e.Status, e.TargetVersion, e.StartCommit, e.EndCommit, stamp(e.CreatedAt), stamp(e.UpdatedAt), e.ActorID)
	if err != nil {
		return err
	}
	if _, err = t.query("UPDATE projects SET updated_at=? WHERE id=?", stamp(e.UpdatedAt), p.ID); err != nil {
		return err
	}
	if _, err = t.query("UPDATE project_tables SET updated_at=? WHERE table_name=?", stamp(e.UpdatedAt), p.TableName); err != nil {
		return err
	}
	return t.db.PutGraphNode(native.GraphNodeInput{GraphName: graph, NodeID: model.EntryNode(p, e.ID), Kind: string(e.Kind), TargetType: native.GraphTargetRecord, TargetNamespace: &p.TableName, TargetRef: &e.ID})
}
func (t *transaction) List(p model.Project, f model.Filter) ([]model.Entry, error) {
	name, err := table(p)
	if err != nil {
		return nil, err
	}
	sql := "SELECT " + columns + " FROM " + name + " WHERE 1=1"
	args := []any{}
	for _, v := range []struct{ column, val string }{{"kind", string(f.Kind)}, {"status", f.Status}, {"target_version", f.TargetVersion}} {
		if v.val != "" {
			sql += " AND " + v.column + "=?"
			args = append(args, v.val)
		}
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	if f.Offset < 0 {
		return nil, model.ErrInvalidInput
	}
	sql += " ORDER BY updated_at DESC,id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, f.Offset)
	rows, err := t.query(sql, args...)
	if err != nil {
		return nil, err
	}
	result := make([]model.Entry, 0, len(rows))
	for _, r := range rows {
		e, err := decode(r)
		if err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, nil
}
func (t *transaction) Link(p model.Project, r model.Relation) error {
	if strings.HasPrefix(r.To, "file:"+p.ID+":") {
		file := strings.TrimPrefix(r.To, "file:"+p.ID+":")
		if err := t.db.PutGraphNode(native.GraphNodeInput{GraphName: graph, NodeID: r.To, Kind: "file", TargetType: native.GraphTargetExternal, TargetNamespace: &p.ID, TargetRef: &file}); err != nil {
			return fmt.Errorf("%w: %w", model.ErrStorage, err)
		}
	}
	if err := t.db.PutGraphEdge(native.GraphEdgeInput{GraphName: graph, FromNodeID: r.From, EdgeType: r.Type, ToNodeID: r.To}); err != nil {
		return fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	return nil
}
func (t *transaction) Unlink(r model.Relation) error {
	has, err := t.db.HasGraphEdge(graph, r.From, r.Type, r.To)
	if err != nil {
		return fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	if !has {
		return model.ErrRelationNotFound
	}
	if err = t.db.DeleteGraphEdge(native.GraphEdgeInput{GraphName: graph, FromNodeID: r.From, EdgeType: r.Type, ToNodeID: r.To}); err != nil {
		return fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	return nil
}
func (t *transaction) Relations(id string) ([]model.Relation, error) {
	ns, err := t.db.GraphNeighbors(native.GraphNeighborsOptions{GraphName: graph, NodeID: id, Direction: native.GraphNeighborOutgoing, Limit: 101})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	result := make([]model.Relation, 0, len(ns))
	for _, n := range ns {
		result = append(result, model.Relation{From: id, Type: n.EdgeType, To: n.NodeID})
	}
	return result, nil
}
