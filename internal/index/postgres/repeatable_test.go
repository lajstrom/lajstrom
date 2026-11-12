package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/internal/sqlstore"
)

// TestRepeatableRead checks Tx under index.WithRepeatableRead on the update of a repo that
// another update overtakes, as a maintainer's PATCH of a proxy's metadataTTL and an admin's
// rename of its upstream do (#791). The maintainer's transaction reads the repo, the admin's
// commits the rename meanwhile, and then the maintainer's writes back the name it read. Under
// WithRepeatableRead the database aborts that write (40001) and Tx runs the maintainer's fn
// again, on the renamed row: both changes stay. A Tx without it is read committed, as every
// other transaction, and runs fn once: the rename is lost. A transaction that finds the row
// changed on every attempt gives up after sqlstore.TxAttempts with an error wrapping
// index.ErrUnavailable.
func TestRepeatableRead(t *testing.T) {
	ctx := t.Context()
	s := openMigrated(t, isolatedDSN(t, testDSN(t)))
	const initial = `{"name": "npmjs", "metadataTTL": "10m"}`
	repo := index.Repo{Format: "npm", Name: "npmjs", Registry: "local", Kind: index.KindProxy,
		Upstream: json.RawMessage(initial)}
	if err := s.CreateRepo(ctx, &repo); err != nil {
		t.Fatal(err)
	}
	rename := func(ctx context.Context) error { return s.Tx(ctx, setUpstream(ctx, "name", "npmjs2", nil)) }
	for _, tt := range []struct {
		name         string
		ctx          context.Context
		meanwhile    func(attempt int) error
		wantAttempts int
		want         string // the upstream after both
		wantState    string // the SQLSTATE of the maintainer's error; "": none
	}{
		{"repeatable read", index.WithRepeatableRead(ctx), func(attempt int) error {
			if attempt > 1 {
				return nil
			}
			return rename(index.WithRepeatableRead(ctx))
		}, 2, `{"name": "npmjs2", "metadataTTL": "1h"}`, ""},
		{"read committed", ctx, func(int) error { return rename(ctx) }, 1, `{"name": "npmjs", "metadataTTL": "1h"}`, ""},
		{"a change on every attempt", index.WithRepeatableRead(ctx), func(attempt int) error {
			return s.Tx(ctx, setUpstream(ctx, "negativeTTL", fmt.Sprint(attempt), nil))
		}, sqlstore.TxAttempts, fmt.Sprintf(`{"name": "npmjs", "metadataTTL": "10m", "negativeTTL": "%d"}`, sqlstore.TxAttempts), "40001"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo.Upstream = json.RawMessage(initial)
			if err := s.UpdateRepo(ctx, &repo); err != nil {
				t.Fatal(err)
			}
			attempts := 0
			err := s.Tx(tt.ctx, func(w index.Writer) error {
				attempts++
				return setUpstream(ctx, "metadataTTL", "1h", func() error { return tt.meanwhile(attempts) })(w)
			})
			switch {
			case tt.wantState == "" && err != nil:
				t.Fatalf("the maintainer's Tx: %v", err)
			case tt.wantState != "" && (!errors.Is(err, index.ErrUnavailable) || sqlState(err) != tt.wantState):
				t.Fatalf("the maintainer's Tx: %v, want index.ErrUnavailable of SQLSTATE %s", err, tt.wantState)
			}
			if attempts != tt.wantAttempts {
				t.Errorf("fn ran %d times, want %d", attempts, tt.wantAttempts)
			}
			got, err := s.GetRepo(ctx, "npm", "npmjs")
			if err != nil {
				t.Fatal(err)
			}
			var gotUp, wantUp map[string]any
			if json.Unmarshal(got.Upstream, &gotUp) != nil || json.Unmarshal([]byte(tt.want), &wantUp) != nil ||
				fmt.Sprint(gotUp) != fmt.Sprint(wantUp) {
				t.Errorf("upstream %s, want %s", got.Upstream, tt.want)
			}
		})
	}
}

// TestIsAborted checks which errors make Tx run a transaction of index.WithRepeatableRead
// again, on Postgres and on YugabyteDB: a serialization failure and a deadlock, wrapped or not.
func TestIsAborted(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"deadlock", fmt.Errorf("%w: repos: %w", index.ErrUnavailable, &pgconn.PgError{Code: "40P01"}), true},
		{"admin shutdown", &pgconn.PgError{Code: "57P01"}, false},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"other", errors.New("boom"), false},
	} {
		for _, d := range []sqlstore.Dialect{dialect, yugabyte} {
			if got := d.IsAborted(tt.err); got != tt.want {
				t.Errorf("%s: IsAborted of %s = %v, want %v", tt.name, d.Name, got, tt.want)
			}
		}
	}
}

// setUpstream returns the fn of a Tx that changes key of the repo npm/npmjs's upstream to value
// as a PATCH does: it reads the repo, calls meanwhile when it is not nil, and writes the repo
// back with the key set in the upstream it read.
func setUpstream(ctx context.Context, key, value string, meanwhile func() error) func(index.Writer) error {
	return func(w index.Writer) error {
		r, err := w.GetRepo(ctx, "npm", "npmjs")
		if err != nil {
			return err
		}
		if meanwhile != nil {
			if err := meanwhile(); err != nil {
				return fmt.Errorf("meanwhile: %w", err)
			}
		}
		var up map[string]any
		if err := json.Unmarshal(r.Upstream, &up); err != nil {
			return err
		}
		up[key] = value
		if r.Upstream, err = json.Marshal(up); err != nil {
			return err
		}
		return w.UpdateRepo(ctx, r)
	}
}
