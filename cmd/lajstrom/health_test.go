package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// pinging is a fake registry whose ping answers err.
type pinging struct {
	*providertest.Fake
	err error
}

func (p *pinging) Ping(context.Context) error { return p.err }

// TestReadyFollowsTheRegistries is the readiness criterion of #23: ready while every registry
// answered GET /v2/ within a minute, not ready once one has not for longer, ready again when it
// answers; a registry without a ping never holds readiness back.
func TestReadyFollowsTheRegistries(t *testing.T) {
	st, err := openStore(t.Context(), config.Database{Driver: "sqlite", DSN: "file:" + filepath.Join(t.TempDir(), "x.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	zot := &pinging{Fake: &providertest.Fake{RegistryName: "zot"}}
	health := newRegistryHealth(provider.Map{"zot": zot, "plain": &providertest.Fake{RegistryName: "plain"}},
		slog.New(slog.DiscardHandler))
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	health.now = func() time.Time { return now }
	check := ready(slog.New(slog.DiscardHandler), "sqlite", st, health, &replica{})

	if err := check(t.Context()); err == nil {
		t.Error("ready before the first ping")
	}
	health.Ping(t.Context())
	if err := check(t.Context()); err != nil {
		t.Errorf("ready after a ping that answered: %v", err)
	}
	zot.err = errors.New("connection refused")
	now = now.Add(30 * time.Second)
	health.Ping(t.Context())
	if err := check(t.Context()); err != nil {
		t.Errorf("ready 30 s after the last answer: %v", err)
	}
	now = now.Add(31 * time.Second)
	health.Ping(t.Context())
	if err := check(t.Context()); err == nil || err.Error() != "a registry has not answered for a minute" {
		t.Errorf("ready 61 s after the last answer: %v", err)
	}
	if stale := health.Stale(); len(stale) != 1 || stale[0] != "zot" {
		t.Errorf("Stale: %v, want [zot]", stale)
	}
	zot.err = nil
	health.Ping(t.Context())
	if err := check(t.Context()); err != nil {
		t.Errorf("ready once it answers again: %v", err)
	}
}

// TestReadyRefusedCredentials: a registry that answers GET /v2/ with 401 answers; its WARN line
// and /-/readyz say that it refuses the credentials, not that it does not answer, and a ping
// that then finds no answer says so in a line of its own (#723).
func TestReadyRefusedCredentials(t *testing.T) {
	st, err := openStore(t.Context(), config.Database{Driver: "sqlite", DSN: "file:" + filepath.Join(t.TempDir(), "x.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var logs bytes.Buffer
	zot := &pinging{Fake: &providertest.Fake{RegistryName: "zot"},
		err: fmt.Errorf("generic: ping: %w (HTTP 401, UNAUTHORIZED)", provider.ErrUnauthorized)}
	health := newRegistryHealth(provider.Map{"zot": zot}, slog.New(slog.NewTextHandler(&logs, nil)))
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	health.now = func() time.Time { return now }
	check := ready(slog.New(slog.DiscardHandler), "sqlite", st, health, &replica{})

	health.Ping(t.Context())
	health.Ping(t.Context())
	if err := check(t.Context()); err == nil || err.Error() != "a registry has refused Lajstrom's credentials for a minute" {
		t.Errorf("readiness with a registry that answers 401: %v", err)
	}
	if n := strings.Count(logs.String(), `msg="registry refuses Lajstrom's credentials at GET /v2/"`); n != 1 ||
		!strings.Contains(logs.String(), "HTTP 401") || strings.Contains(logs.String(), "does not answer") {
		t.Errorf("the log of two pings answered 401, want one line that names the 401:\n%s", &logs)
	}
	zot.err = errors.New("connection refused")
	health.Ping(t.Context())
	if err := check(t.Context()); err == nil || err.Error() != "a registry has not answered for a minute" {
		t.Errorf("readiness once it does not answer: %v", err)
	}
	if !strings.Contains(logs.String(), `msg="registry does not answer GET /v2/"`) {
		t.Errorf("no line for the registry that stopped answering:\n%s", &logs)
	}
}
