package mcp

import (
	"errors"
	"fmt"
	"testing"

	"elephant/internal/model"
)

func TestPublicErrorKeepsKnownErrors(t *testing.T) {
	for _, known := range []error{model.ErrEvidenceNotFound, model.ErrEntryNotFound, model.ErrInvalidInput} {
		err := fmt.Errorf("wrapped: %w", known)
		if got := PublicError(err); !errors.Is(got, known) {
			t.Fatalf("PublicError(%v) = %v, want %v", err, got, known)
		}
	}
	if got := PublicError(errors.New("native detail")); !errors.Is(got, model.ErrStorage) {
		t.Fatalf("unknown error mapped to %v, want storage error", got)
	}
}
