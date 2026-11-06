package index

import (
	"errors"
	"fmt"
	"testing"
)

func TestTagConflictError(t *testing.T) {
	err := fmt.Errorf("upsert version: %w", &TagConflictError{Tag: "1.2.0", Existing: "sha256:abc"})

	if !errors.Is(err, ErrConflict) {
		t.Errorf("errors.Is(%v, ErrConflict) = false", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("errors.Is(%v, ErrNotFound) = true", err)
	}
	var tc *TagConflictError
	if !errors.As(err, &tc) || tc.Existing != "sha256:abc" {
		t.Errorf("errors.As(%v) = %v, want the existing digest", err, tc)
	}
	if want := `upsert version: index: tag "1.2.0" already points at sha256:abc`; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}
