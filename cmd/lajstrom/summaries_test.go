package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// summaryStore is the index of the tests of fillSummaries: versions rows by id, each with its
// summary, nil when it was never written, as the store's ListVersionsWithoutSummary and
// SetVersionSummaries treat them (the sqlite package tests the statements).
type summaryStore struct {
	index.Store
	mu        sync.Mutex
	rows      map[int64]index.Version
	summaries map[int64]*string
	fail      error
}

func (s *summaryStore) ListVersionsWithoutSummary(_ context.Context, afterID int64, limit int) ([]index.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	out := []index.Version{}
	for _, id := range slices.Sorted(maps.Keys(s.rows)) {
		if id > afterID && s.summaries[id] == nil && s.rows[id].Metadata != nil && len(out) < limit {
			out = append(out, s.rows[id])
		}
	}
	return out, nil
}

func (s *summaryStore) SetVersionSummaries(_ context.Context, summaries map[int64]string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, summary := range summaries {
		if v, ok := s.rows[id]; ok && s.summaries[id] == nil && v.Metadata != nil {
			s.summaries[id] = &summary
			n++
		}
	}
	return n, nil
}

// TestFillSummaries: the rows written before migration 0029, whose summary was never written,
// get the summary of their metadata, empty for a format without one, a few at a time; a row
// without metadata keeps none. Two fills at once, as two replicas starting, write each row once
// and agree, and a later fill finds nothing and says nothing.
func TestFillSummaries(t *testing.T) {
	st := &summaryStore{rows: map[int64]index.Version{}, summaries: map[int64]*string{}}
	want := map[int64]*string{}
	for i, r := range []struct{ typ, metadata, summary string }{
		{"pypi", `{"summary": "Python HTTP for Humans."}`, "Python HTTP for Humans."},
		{"maven", `{"groupId": "com.example"}`, ""},
		{"deb", `{"control": {"Description": "example package\n long part"}}`, "example package"},
		{"pypi", `{"name": "requests"}`, ""},
		{"rpm", `{"summary": "The GNU Bourne Again shell"}`, "The GNU Bourne Again shell"},
		{"npm", ``, ""},
	} {
		id := int64(10 + i)
		v := index.Version{ID: id, ArtifactType: profile.ArtifactType(r.typ)}
		if r.metadata != "" {
			v.Metadata, want[id] = json.RawMessage(r.metadata), &r.summary
		}
		st.rows[id] = v
	}
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { fillSummaries(t.Context(), st, log, 2) })
	}
	wg.Wait()
	for id, w := range want {
		if got := st.summaries[id]; got == nil || *got != *w {
			t.Errorf("row %d: summary %v, want %q", id, got, *w)
		}
	}
	if got := st.summaries[15]; got != nil {
		t.Errorf("the row without metadata has the summary %q", *got)
	}
	if written := loggedWritten(t, logs.String()); written != 5 {
		t.Errorf("the two fills wrote %d rows, want 5:\n%s", written, logs.String())
	}

	var again syncBuffer
	fillSummaries(t.Context(), st, slog.New(slog.NewTextHandler(&again, nil)), 2)
	if again.String() != "" {
		t.Errorf("a fill with nothing left logged %q", again.String())
	}
}

// loggedWritten adds up the rows written that the lines of the fills say.
func loggedWritten(t *testing.T, logs string) int {
	t.Helper()
	written := 0
	for line := range strings.Lines(logs) {
		_, rest, ok := strings.Cut(line, `msg="the summaries of the versions before migration 0029 filled from their metadata"`)
		var read, n, with int
		if _, err := fmt.Sscanf(rest, " read=%d written=%d withSummary=%d", &read, &n, &with); !ok || err != nil {
			t.Errorf("a fill logged %q", line)
		}
		written += n
	}
	return written
}

// TestFillSummariesFails: a fill that cannot read says so and stops.
func TestFillSummariesFails(t *testing.T) {
	st := &summaryStore{fail: errors.New("database gone")}
	var logs syncBuffer
	fillSummaries(t.Context(), st, slog.New(slog.NewTextHandler(&logs, nil)), 2)
	if !strings.Contains(logs.String(), `level=WARN msg="the summaries of the versions before migration 0029 are not all filled; `+
		`the next start goes on" read=0 written=0 error="database gone"`) {
		t.Errorf("the log of a fill that fails: %q", logs.String())
	}
}
