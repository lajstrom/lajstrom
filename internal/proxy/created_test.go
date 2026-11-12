package proxy_test

import (
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/proxy"
)

// TestFileCreated: a version fetched from an upstream that gives its publish time is dated by
// it, org.opencontainers.image.created and versions.created_at, so that it keeps its date when
// the proxy becomes a hosted repo (docs/spec/manifest-profile.md §3.1); without one it keeps
// the time Build gave it.
func TestFileCreated(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "one", at("2025-06-01T10:20:30Z"))
	e.up.publish("tool", "2.0.0", "two", nil)
	c := e.core(t, "r1", proxy.Options{})

	m, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0"))
	must(t, err)
	if want := time.Date(2025, 6, 1, 10, 20, 30, 0, time.UTC); !m.Version.CreatedAt.Equal(want) {
		t.Errorf("1.0.0: CreatedAt %s, want the upstream's %s", m.Version.CreatedAt, want)
	}
	m, err = c.File(asAlice(t), e.repo, fileOf("tool", "2.0.0"))
	must(t, err)
	if !m.Version.CreatedAt.Equal(created) {
		t.Errorf("2.0.0: CreatedAt %s, want Build's %s", m.Version.CreatedAt, created)
	}
}
