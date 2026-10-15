package zova

import (
	"errors"
	"fmt"
	"strings"

	native "github.com/ata-sesli/zova/bindings/go"

	"elephant/internal/model"
)

// escapeLike escapes SQL LIKE wildcards so text search is literal and bounded.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// Search implements deterministic bounded text retrieval with LIKE. Results
// sort newest first with ID tie-break; callers bound page size.
func (t *transaction) Search(p model.Project, q model.SearchQuery) ([]model.Entry, error) {
	name, err := table(p)
	if err != nil {
		return nil, err
	}
	if q.Kind != "" && q.Kind != model.Fact && q.Kind != model.Decision && q.Kind != model.Task {
		return nil, model.ErrInvalidKind
	}
	if q.Status != "" && !model.ValidStatus(q.Kind, q.Status) {
		return nil, model.ErrInvalidStatus
	}
	limit := q.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if q.Offset < 0 {
		return nil, model.ErrInvalidInput
	}
	sql := "SELECT " + columns + " FROM " + name + " WHERE 1=1"
	args := []any{}
	if q.Kind != "" {
		sql += " AND kind=?"
		args = append(args, string(q.Kind))
	}
	if q.Status != "" {
		sql += " AND status=?"
		args = append(args, q.Status)
	}
	if q.Actor != "" {
		sql += " AND actor_id=?"
		args = append(args, q.Actor)
	}
	if q.TargetVersion != "" {
		sql += " AND target_version=?"
		args = append(args, q.TargetVersion)
	}
	if q.UpdatedAfter != nil {
		sql += " AND updated_at>=?"
		args = append(args, stamp(*q.UpdatedAfter))
	}
	if q.UpdatedBefore != nil {
		sql += " AND updated_at<=?"
		args = append(args, stamp(*q.UpdatedBefore))
	}
	if strings.TrimSpace(q.Query) != "" {
		like := "%" + escapeLike(strings.TrimSpace(q.Query)) + "%"
		sql += ` AND (title LIKE ? ESCAPE '\' OR body LIKE ? ESCAPE '\')`
		args = append(args, like, like)
	}
	sql += " ORDER BY updated_at DESC,id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, q.Offset)
	rows, err := t.query(sql, args...)
	if err != nil {
		return nil, err
	}
	out := make([]model.Entry, 0, len(rows))
	for _, r := range rows {
		e, err := decode(r)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Incoming lists edges pointing at a node, used for file history and reverse
// graph walks. Results are capped to keep traversal bounded.
func (t *transaction) Incoming(id string) ([]model.Relation, error) {
	ns, err := t.db.GraphNeighbors(native.GraphNeighborsOptions{GraphName: graph, NodeID: id, Direction: native.GraphNeighborIncoming, Limit: 101})
	var nativeErr *native.Error
	if errors.As(err, &nativeErr) && nativeErr.Status == native.StatusGraphNodeNotFound {
		// A file or entry node that was never linked has no incoming edges.
		return []model.Relation{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", model.ErrStorage, err)
	}
	out := make([]model.Relation, 0, len(ns))
	for _, n := range ns {
		out = append(out, model.Relation{From: n.NodeID, Type: n.EdgeType, To: id})
	}
	return out, nil
}
