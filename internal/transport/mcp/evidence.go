package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"elephant/internal/app"
)

type evidenceAddInput struct {
	scope
	EntryID string `json:"entry_id" jsonschema:"ID of the fact this evidence supports"`
	Path    string `json:"path" jsonschema:"File that established the fact, relative to cwd like a Git pathspec"`
	Line    int    `json:"line,omitempty" jsonschema:"Optional one-based line number inside the file"`
}
type evidenceEntryInput struct {
	scope
	EntryID string `json:"entry_id" jsonschema:"Fact entry ID whose evidence should be read"`
}
type evidenceRefreshInput struct {
	scope
	EntryID    string `json:"entry_id" jsonschema:"Fact entry ID whose evidence should be refreshed"`
	EvidenceID string `json:"evidence_id,omitempty" jsonschema:"Refresh only this evidence row; omit to refresh every row of the entry"`
}
type evidenceRemoveInput struct {
	scope
	EvidenceID string `json:"evidence_id" jsonschema:"ID of the evidence row to delete"`
}

// registerEvidence exposes deterministic evidence operations. Verification is
// read-only; only refresh and remove write, and neither changes entry state.
func registerEvidence(s *sdk.Server, service *app.Service, cwd func(string) string) {
	register(s, "add_evidence", "Attach a file (relative to cwd, stored repository-relative) and optional line to a fact as verifiable evidence, capturing the current commit and the stable digest read from that commit. The fact's lifecycle state is unchanged.", func(ctx context.Context, in evidenceAddInput) (app.EvidenceView, error) {
		return service.AddEvidence(ctx, cwd(in.CWD), in.EntryID, in.Path, in.Line)
	})
	register(s, "list_evidence", "List a fact's recorded evidence with its deterministic state: unchanged, changed, missing, or unavailable.", func(ctx context.Context, in evidenceEntryInput) ([]app.EvidenceView, error) {
		return service.ListEvidence(ctx, cwd(in.CWD), in.EntryID)
	})
	register(s, "verify_evidence", "Re-verify a fact's evidence against the working tree without writing anything, reporting per-state counts.", func(ctx context.Context, in evidenceEntryInput) (app.EvidenceReport, error) {
		return service.VerifyEvidence(ctx, cwd(in.CWD), in.EntryID)
	})
	register(s, "refresh_evidence", "Re-capture the commit digest and verification time for a fact's evidence after an intentional review. Stored evidence and entry state are otherwise unchanged.", func(ctx context.Context, in evidenceRefreshInput) (app.EvidenceReport, error) {
		return service.RefreshEvidence(ctx, cwd(in.CWD), in.EntryID, in.EvidenceID)
	})
	register(s, "remove_evidence", "Remove one recorded evidence location from a fact.", func(ctx context.Context, in evidenceRemoveInput) (map[string]string, error) {
		return service.RemoveEvidence(ctx, cwd(in.CWD), in.EvidenceID)
	})
}
