// Package model defines Elephant's transport-independent continuity records.
package model

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

var (
	ErrProjectNotFound   = errors.New("project not found")
	ErrEntryNotFound     = errors.New("entry not found in this project")
	ErrInvalidKind       = errors.New("invalid entry kind")
	ErrInvalidStatus     = errors.New("invalid status for entry kind")
	ErrInvalidTransition = errors.New("invalid lifecycle transition")
	ErrInvalidInput      = errors.New("invalid input")
	ErrRelationNotFound  = errors.New("relation not found")
	ErrEvidenceNotFound  = errors.New("evidence not found")
	ErrRemote            = errors.New("remote operation failed")
	ErrStorage           = errors.New("storage operation failed")
	ErrSchema            = errors.New("incompatible Elephant schema; explicit migration required")
	ErrMigration         = errors.New("database format migration failed")
	ErrDiagnostics       = errors.New("diagnostic checks failed")
)

type Kind string

const (
	Fact     Kind = "fact"
	Decision Kind = "decision"
	Task     Kind = "task"
)

type Project struct {
	ID               string    `json:"id"`
	Identity         string    `json:"identity"`
	Name             string    `json:"name"`
	Remote           string    `json:"remote,omitempty"`
	Scope            string    `json:"scope,omitempty"`
	SourceElephantID string    `json:"source_elephant_id,omitempty"`
	TableName        string    `json:"table_name"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}
type Entry struct {
	ActorID       string    `json:"actor_id"`
	ID            string    `json:"id"`
	Kind          Kind      `json:"kind"`
	Title         string    `json:"title"`
	Body          string    `json:"body"`
	Status        string    `json:"status"`
	TargetVersion *string   `json:"target_version"`
	StartCommit   *string   `json:"start_commit"`
	EndCommit     *string   `json:"end_commit"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
type Relation struct {
	From string `json:"from"`
	Type string `json:"type"`
	To   string `json:"to"`
}
type Filter struct {
	Kind          Kind   `json:"kind,omitempty"`
	Status        string `json:"status,omitempty"`
	TargetVersion string `json:"target_version,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	Offset        int    `json:"offset,omitempty"`
}

func DefaultStatus(k Kind) string {
	if k == Task {
		return "open"
	}
	return "active"
}
func ValidStatus(k Kind, s string) bool {
	switch k {
	case Fact:
		return s == "active" || s == "stale" || s == "retired"
	case Decision:
		return s == "active" || s == "superseded" || s == "retired"
	case Task:
		return s == "open" || s == "active" || s == "blocked" || s == "done" || s == "cancelled"
	}
	return false
}
func Terminal(k Kind, s string) bool {
	return s == "retired" || s == "superseded" || (k == Task && (s == "done" || s == "cancelled"))
}
func (e Entry) Validate() error {
	if e.Kind != Fact && e.Kind != Decision && e.Kind != Task {
		return ErrInvalidKind
	}
	if !ValidStatus(e.Kind, e.Status) {
		return ErrInvalidStatus
	}
	if strings.TrimSpace(e.Title) == "" || strings.TrimSpace(e.Body) == "" || len(e.Title) > 300 || len(e.Body) > 32768 || strings.ContainsRune(e.Title+e.Body, 0) {
		return fmt.Errorf("%w: title (1–300 bytes) and body (1–32768 bytes) required", ErrInvalidInput)
	}
	if e.TargetVersion != nil && (len(*e.TargetVersion) > 200 || strings.ContainsRune(*e.TargetVersion, 0)) {
		return fmt.Errorf("%w: target_version too long or contains NUL", ErrInvalidInput)
	}
	return nil
}
func ValidateTransition(k Kind, from, to string) error {
	if !ValidStatus(k, to) {
		return ErrInvalidStatus
	}
	if from != to && Terminal(k, from) {
		return ErrInvalidTransition
	}
	return nil
}
func CleanFile(s string) (string, error) {
	p := path.Clean(s)
	if p == "." || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsAny(s, "\\:\x00") || len(p) > 4096 {
		return "", fmt.Errorf("%w: file must be a repository-relative path", ErrInvalidInput)
	}
	return p, nil
}
func FileEdge(k Kind) string {
	switch k {
	case Fact:
		return "concerns"
	case Decision:
		return "affects"
	case Task:
		return "modifies"
	}
	return ""
}
func ValidateRelation(a Kind, edge string, b Kind) error {
	if (a == Decision && edge == "supersedes" && b == Decision) || (a == Task && edge == "depends_on" && b == Task) || (a == Task && edge == "implements" && b == Decision) || (a == Fact && edge == "supports" && b == Decision) {
		return nil
	}
	return fmt.Errorf("%w: relation is not valid for these entry kinds", ErrInvalidInput)
}
