package format_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestPointersOf: the dist-tags of the state that name the version, and nothing else.
func TestPointersOf(t *testing.T) {
	state := []index.JournalState{
		{Kind: index.JournalDeprecated, Key: "1.0.0", Value: "use 2"},
		{Kind: index.JournalDistTag, Key: "beta", Value: "1.0.0"},
		{Kind: index.JournalDistTag, Key: "latest", Value: "2.0.0"},
		{Kind: index.JournalDistTag, Key: "lts", Value: "1.0.0"},
	}
	got := format.PointersOf(state, "1.0.0")
	if want := []format.Pointer{{Kind: "dist-tag", Name: "beta"}, {Kind: "dist-tag", Name: "lts"}}; !slices.Equal(got, want) {
		t.Errorf("PointersOf 1.0.0 = %v, want %v", got, want)
	}
	if got := format.PointersOf(state, "3.0.0"); got != nil {
		t.Errorf("PointersOf 3.0.0 = %v, want none", got)
	}
}

// TestRefusedError: each refusal says what failed and that nothing was deleted, and wraps the
// sentinel of its status.
func TestRefusedError(t *testing.T) {
	subject := `version 1.0 of "requests" in pypi/libs`
	for _, tt := range []struct {
		err      *format.RefusedError
		sentinel error
		want     string
	}{
		{&format.RefusedError{Reason: format.RefusedChanged, Subject: subject, Digests: []digest.Digest{"sha256:aa", "sha256:bb"}},
			format.ErrPrecondition, "changed since it was read: its variants are sha256:aa, sha256:bb now"},
		{&format.RefusedError{Reason: format.RefusedYoung, Subject: subject, PublishedAt: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)},
			format.ErrPrecondition, "was published at 2026-11-02T09:00:00Z, after the cut-off"},
		{&format.RefusedError{Reason: format.RefusedProtected, Subject: subject, Pointers: []format.Pointer{{Kind: "dist-tag", Name: "latest"}}},
			index.ErrConflict, "is what dist-tag latest points to; move it, or force the delete"},
	} {
		msg := tt.err.Error()
		if !strings.HasPrefix(msg, subject) || !strings.Contains(msg, tt.want) || !strings.HasSuffix(msg, "nothing was deleted") {
			t.Errorf("%s: %q, want it to contain %q", tt.err.Reason, msg, tt.want)
		}
		if !errors.Is(tt.err, tt.sentinel) {
			t.Errorf("%s does not wrap %v", tt.err.Reason, tt.sentinel)
		}
	}
}
