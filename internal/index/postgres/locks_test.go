package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// idleTimeout is the idle_in_transaction_session_timeout of the role of TestReferrersLockLost.
const idleTimeout = time.Second

// TestReferrersLockLost runs the referrers lock as a role whose
// idle_in_transaction_session_timeout is short, the setting that ended the lock's transaction
// while its function still ran, so that a second writer held the lock too and the first one's
// WithReferrersLock returned nil (#615). The lock's transaction outlives the timeout: a
// second writer enters only after the first one's function returned. When the lock's backend
// is terminated anyway, WithReferrersLock returns an error wrapping index.ErrUnavailable,
// never nil, and the next lock is taken on a new connection. The role is the test's own, so
// that the timeout touches no other session of the database.
func TestReferrersLockLost(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	l := newLockRole(ctx, t)

	// A plain transaction of the role idles while the lock is held below: the timeout must end
	// it, else this test proves nothing.
	plain, err := openDB(t, l.dsn).BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	t.Run("outlives the timeout", func(t *testing.T) { l.outlives(ctx, t) })
	if err := plain.Commit(); err == nil {
		t.Fatalf("a transaction of the role idled %v past its idle_in_transaction_session_timeout and committed", 3*idleTimeout)
	}
	t.Run("terminated", func(t *testing.T) { l.terminated(ctx, t) })
}

// lockRole is a store that logs in as a role of its own, whose
// idle_in_transaction_session_timeout is idleTimeout, and the lock it takes.
type lockRole struct {
	role, dsn string
	admin     *sql.DB // the test DSN's user
	s         *Store
	name      string
	subject   digest.Digest
}

// newLockRole creates the role, which the test drops when it ends, and opens the store as it.
func newLockRole(ctx context.Context, t *testing.T) *lockRole {
	t.Helper()
	dsn := testDSN(t)
	role, password := newRole(t, dsn)
	l := &lockRole{role: role, dsn: withUser(t, dsn, role, password), admin: openDB(t, dsn),
		name: "raw/" + role, subject: digest.FromString("subject")}
	if _, err := l.admin.ExecContext(ctx, "ALTER ROLE "+role+" SET idle_in_transaction_session_timeout = "+
		strconv.FormatInt(idleTimeout.Milliseconds(), 10)); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, l.dsn) // the lock needs no table: no schema, no migration
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	l.s = s
	return l
}

// outlives holds the lock three times the timeout while a second writer waits for it: the
// second one enters after the first one's function returned, and both succeed.
func (l *lockRole) outlives(ctx context.Context, t *testing.T) {
	var firstEnd, secondStart time.Time
	entered, first := make(chan struct{}), make(chan error, 1)
	go func() {
		first <- l.s.WithReferrersLock(ctx, l.name, l.subject, func(context.Context) error {
			close(entered)
			time.Sleep(3 * idleTimeout)
			firstEnd = time.Now()
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-first:
		t.Fatalf("the first writer: %v", err)
	}
	err := l.s.WithReferrersLock(ctx, l.name, l.subject, func(context.Context) error {
		secondStart = time.Now()
		return nil
	})
	if err != nil {
		t.Errorf("the second writer: %v", err)
	}
	if err := <-first; err != nil {
		t.Errorf("the first writer: %v", err)
	}
	if secondStart.Before(firstEnd) {
		t.Errorf("the second writer entered %v before the first one's function returned: the timeout ended the lock",
			firstEnd.Sub(secondStart).Round(time.Millisecond))
	}
}

// terminated ends the lock's backend from its function, as a server that shuts down does: the
// error wraps index.ErrUnavailable and says that the lock was lost.
func (l *lockRole) terminated(ctx context.Context, t *testing.T) {
	n := -1
	err := l.s.WithReferrersLock(ctx, l.name, l.subject, func(ctx context.Context) error {
		// The lock's transaction is the role's one session that idles in a transaction.
		err := l.admin.QueryRowContext(ctx, "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity"+
			" WHERE usename = $1 AND state = 'idle in transaction'", l.role).Scan(&n)
		if err != nil {
			t.Errorf("pg_terminate_backend: %v", err)
		}
		return nil
	})
	if n != 1 {
		t.Fatalf("pg_terminate_backend ended %d sessions, want the lock's one", n)
	}
	if !errors.Is(err, index.ErrUnavailable) || !strings.Contains(err.Error(), "was lost") {
		t.Fatalf("WithReferrersLock after its backend was terminated: %v, want index.ErrUnavailable saying the lock was lost", err)
	}
	if err := l.s.WithReferrersLock(ctx, l.name, l.subject, func(context.Context) error { return nil }); err != nil {
		t.Errorf("the lock after the termination: %v", err)
	}
}
