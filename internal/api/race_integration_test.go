//go:build integration

package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/postgres"
)

// TestIntegrationUpdateRace sends a maintainer's change of a proxy's metadataTTL and an admin's
// rename of its upstream at once, on Postgres (#791). The maintainer's Update reads the repo,
// and the admin's commits before it writes. The maintainer's transaction is then run again on
// the renamed repo: both changes stay, and each Update leaves one audit event, the
// maintainer's with the settings it committed. Read committed, the maintainer's write put
// the old name back. It needs LAJSTROM_TEST_POSTGRES_DSN, a database it may migrate; its repo
// has a name of its own.
func TestIntegrationUpdateRace(t *testing.T) {
	dsn := os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set LAJSTROM_TEST_POSTGRES_DSN to run against Postgres")
	}
	ctx := context.Background()
	st, err := postgres.Open(ctx, dsn)
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repos := func(ix index.Store) *Repos {
		return &Repos{
			Index: ix, Formats: oneOf("npm"), Registries: oneOf("local"),
			Proxiers: func(name string) (format.Proxier, bool) { return stubProxier{}, name == "npm" },
			Upstreams: func(name string) (string, bool) {
				return "npm", name == "npmjs" || name == "npmjs2"
			},
		}
	}
	admin := repos(st)
	name := "race-" + strings.ToLower(rand.Text())
	_, err = admin.Create(ctx, CreateRequest{Format: "npm", Name: name, Registry: "local", Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "npmjs"}`)}, Origin{})
	must(t, err)

	attempts := 0
	maintainer := repos(readHook{Store: st, hook: func() {
		if attempts++; attempts == 1 {
			_, err := admin.Update(ctx, "npm", name, UpdateRequest{Upstream: json.RawMessage(`{"name": "npmjs2"}`), Admin: true}, Origin{})
			must(t, err)
		}
	}})
	got, err := maintainer.Update(ctx, "npm", name, UpdateRequest{Upstream: json.RawMessage(`{"metadataTTL": "1h"}`)}, Origin{})
	must(t, err)
	if attempts != 2 {
		t.Errorf("the maintainer's transaction ran %d times, want 2", attempts)
	}
	want := `{"name":"npmjs2","metadataTTL":"1h","negativeTTL":"2m","offline":false,"allow":[],"deny":[]}`
	wantJSON(t, "upstream that the maintainer's Update returned", got.Upstream, want)
	row, err := st.GetRepo(ctx, "npm", name)
	must(t, err)
	wantJSON(t, "upstream after both", row.Upstream, want)
	evs, err := st.ListAudit(ctx, index.AuditFilter{Repo: "npm/" + name, Action: actionUpdate})
	must(t, err)
	if len(evs) != 2 {
		t.Fatalf("%d repo.update audit events, want 2, the admin's and the maintainer's", len(evs))
	}
	var details struct{ Changed map[string]json.RawMessage }
	must(t, json.Unmarshal(evs[0].Details, &details)) // newest first: the maintainer's
	wantJSON(t, "the maintainer's audit details", details.Changed, `{"upstream":`+want+`}`)
}

// readHook is a store whose transactions call hook after each GetRepo that found the repo.
type readHook struct {
	index.Store
	hook func()
}

func (s readHook) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(hookedWriter{w, s.hook}) })
}

// hookedWriter is a Writer whose GetRepo calls hook after a read that found the repo.
type hookedWriter struct {
	index.Writer
	hook func()
}

func (w hookedWriter) GetRepo(ctx context.Context, formatName, name string) (*index.Repo, error) {
	r, err := w.Writer.GetRepo(ctx, formatName, name)
	if err == nil {
		w.hook()
	}
	return r, err
}

// TestIntegrationConditionalRace sends two changes of a repo with the same If-Match at once, on
// Postgres (#796): the first reads the repo, and the second, a PATCH or a DELETE, commits
// before the first writes. The first's transaction is then run again on the repo as the
// second left it: 412 with the ETag now after a PATCH, 404 after a DELETE. Exactly one change
// is made.
func TestIntegrationConditionalRace(t *testing.T) {
	dsn := os.Getenv("LAJSTROM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set LAJSTROM_TEST_POSTGRES_DSN to run against Postgres")
	}
	ctx := context.Background()
	st, err := postgres.Open(ctx, dsn)
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	for _, second := range []string{"PATCH", "DELETE"} {
		t.Run(second, func(t *testing.T) {
			other := &Repos{Index: st, Formats: oneOf("raw"), Registries: oneOf("local")}
			name := "cond-" + strings.ToLower(rand.Text())
			created, err := other.Create(ctx, CreateRequest{Format: "raw", Name: name, Registry: "local"}, Origin{})
			must(t, err)
			cond, err := IfMatch(TableRepos, "repo raw/"+name, created.ETag())
			must(t, err)
			cctx := index.WithCondition(ctx, cond)
			attempts := 0
			first := &Repos{Index: readHook{Store: st, hook: func() {
				if attempts++; attempts > 1 {
					return
				}
				if second == "PATCH" {
					_, err := other.Update(cctx, "raw", name, UpdateRequest{Policy: json.RawMessage(`{"maxFileSize": 1}`)}, Origin{})
					must(t, err)
				} else {
					must(t, other.Delete(cctx, "raw", name, false, Origin{}))
				}
			}}, Formats: oneOf("raw"), Registries: oneOf("local")}
			anon := true
			_, err = first.Update(cctx, "raw", name, UpdateRequest{AnonymousRead: &anon}, Origin{})
			if second == "PATCH" && attempts != 2 {
				t.Errorf("the first transaction ran %d times, want 2", attempts)
			}
			row, rerr := st.GetRepo(ctx, "raw", name)
			var pe *PreconditionError
			switch {
			case second == "PATCH" && (!errors.As(err, &pe) || rerr != nil || pe.ETag != ETag(TableRepos, row.ID, row.Revision)):
				t.Errorf("the first PATCH: %v, the repo %+v (%v); want 412 with the ETag the second left", err, row, rerr)
			case second == "PATCH" && (row.AnonymousRead || row.Revision != 2):
				t.Errorf("the repo after both: %+v, want the second's change alone", row)
			case second == "DELETE" && (!errors.Is(err, index.ErrNotFound) || !errors.Is(rerr, index.ErrNotFound)):
				t.Errorf("the first PATCH after the delete: %v, the repo %v; want both not found", err, rerr)
			}
		})
	}
}
