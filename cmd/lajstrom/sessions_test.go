package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

// syncBuffer is a bytes.Buffer that a logger may write while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestExpireSessions: the loop of serve marks a session whose time is up as expired, says so,
// and stops with its context.
func TestExpireSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(t.TempDir(), "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := index.Repo{Format: "maven", Name: "releases", Registry: "local", Kind: "hosted"}
	if err := st.CreateRepo(ctx, &repo); err != nil {
		t.Fatal(err)
	}
	s := index.Session{RepoID: repo.ID, Key: "com.example:lib:1.0", Principal: "user:alice", State: "open",
		ExpiresAt: time.Now().Add(-time.Minute)}
	if err := st.UpsertSession(ctx, &s); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	done := make(chan struct{})
	go func() {
		expireSessions(ctx, ingest.New(ingest.Options{Index: st}), slog.New(slog.NewTextHandler(&logs, nil)), 10*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := st.GetSessionByID(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == "expired" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session is %q after 5 s, want expired", got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if !strings.Contains(logs.String(), `msg="sessions expired: their files are dropped" count=1`) {
		t.Errorf("the log does not say so:\n%s", logs.String())
	}
}
