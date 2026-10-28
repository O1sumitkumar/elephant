package mcp

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"elephant/internal/app"
	"elephant/internal/model"
)

type searchInput struct {
	scope
	Query         string     `json:"query,omitempty"`
	Kind          model.Kind `json:"kind,omitempty"`
	Status        string     `json:"status,omitempty"`
	Actor         string     `json:"actor,omitempty"`
	TargetVersion string     `json:"target_version,omitempty"`
	Commit        string     `json:"commit,omitempty"`
	CommitStart   string     `json:"commit_start,omitempty"`
	CommitEnd     string     `json:"commit_end,omitempty"`
	File          string     `json:"file,omitempty"`
	Remote        string     `json:"remote,omitempty"`
	UpdatedAfter  string     `json:"updated_after,omitempty"`
	UpdatedBefore string     `json:"updated_before,omitempty"`
	Limit         int        `json:"limit,omitempty"`
	Offset        int        `json:"offset,omitempty"`
}

// parseTimeFilters converts RFC3339 timestamp strings to pointers; empty
// strings yield nil filters so both bounds stay optional.
func parseTimeFilters(after, before string) (*time.Time, *time.Time, error) {
	if after == "" && before == "" {
		return nil, nil, nil
	}
	var a, b *time.Time
	if after != "" {
		t, err := time.Parse(time.RFC3339, after)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: updated_after requires RFC3339: %v", model.ErrInvalidInput, err)
		}
		a = &t
	}
	if before != "" {
		t, err := time.Parse(time.RFC3339, before)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: updated_before requires RFC3339: %v", model.ErrInvalidInput, err)
		}
		b = &t
	}
	return a, b, nil
}

type relatedInput struct {
	scope
	ID        string `json:"id"`
	Direction string `json:"direction,omitempty"`
	Edge      string `json:"edge,omitempty"`
	Depth     int    `json:"depth,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type historyInput struct {
	scope
	File  string `json:"file"`
	Limit int    `json:"limit,omitempty"`
}

func registerSearch(s *sdk.Server, service *app.Service, cwd func(string) string) {
	register(s, "search_entries", "Deterministic text search with composable filters and match metadata; project-scoped with stable ordering. updated_after/updated_before take RFC3339 timestamps. commit/commit_start/commit_end request an inclusive Git commit range resolved in the local repository (entries match when their recorded commit interval intersects it); an unresolvable bound, reversed endpoints, or endpoints on divergent histories are invalid-input errors.", func(ctx context.Context, in searchInput) ([]app.SearchResult, error) {
		after, before, err := parseTimeFilters(in.UpdatedAfter, in.UpdatedBefore)
		if err != nil {
			return nil, err
		}
		return service.Search(ctx, cwd(in.CWD), app.SearchFilters{Query: in.Query, Kind: in.Kind, Status: in.Status, Actor: in.Actor, TargetVersion: in.TargetVersion, Commit: in.Commit, CommitStart: in.CommitStart, CommitEnd: in.CommitEnd, File: in.File, Source: in.Remote, UpdatedAfter: after, UpdatedBefore: before, Limit: in.Limit, Offset: in.Offset})
	})
	register(s, "related_entries", "Bounded graph traversal from one entry with direction, edge type, depth, and limits; never crosses project boundaries.", func(ctx context.Context, in relatedInput) ([]app.RelatedResult, error) {
		depth := in.Depth
		if depth == 0 {
			depth = 1
		}
		return service.Related(ctx, cwd(in.CWD), in.ID, in.Direction, in.Edge, depth, in.Limit)
	})
	register(s, "file_history", "List entries linked to one file, relative to cwd like a Git pathspec, newest first.", func(ctx context.Context, in historyInput) ([]model.Entry, error) {
		return service.History(ctx, cwd(in.CWD), in.File, in.Limit)
	})
}
