package format_test

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestPartial: a delete that failed part way keeps its failure's status, and its answer names
// what it deleted, whatever the status; nothing deleted is the failure itself.
func TestPartial(t *testing.T) {
	deleted := []string{"1.0 (sha256:aa)", "1.1 (sha256:bb)"}
	names := "deleted before the failure, and still deleted: 1.0 (sha256:aa), 1.1 (sha256:bb)"
	for _, tt := range []struct {
		err    error
		status int
		body   string
	}{
		{fmt.Errorf("raw: delete 1.2: %w", provider.ErrConflict), 409, "raw: delete 1.2: provider: conflict; " + names + "\n"},
		{errors.New("index: gone"), 500, "Internal Server Error; " + names + "\n"},
	} {
		err := format.Partial(tt.err, deleted)
		if !errors.Is(err, tt.err) {
			t.Errorf("%v does not wrap %v", err, tt.err)
		}
		rr := httptest.NewRecorder()
		if got := format.WriteError(rr, err); got != tt.status || rr.Body.String() != tt.body {
			t.Errorf("WriteError = %d %q, want %d %q", got, rr.Body, tt.status, tt.body)
		}
	}
	var p *format.PartialError
	if err := errors.New("boom"); errors.As(format.Partial(err, nil), &p) || !errors.Is(format.Partial(err, nil), err) || format.Partial(nil, deleted) != nil {
		t.Error("Partial of nothing deleted, or of no error, is not the error itself")
	}
}
