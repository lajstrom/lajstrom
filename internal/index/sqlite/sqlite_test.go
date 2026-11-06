package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/storetest"
)

// memoryDBs numbers the in-memory databases, so that every test gets its own.
var memoryDBs atomic.Int64

// openMigrated opens dsn, migrates it and closes it when the test ends.
func openMigrated(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestConformance runs the store suite on in-memory databases, one per subtest. A shared-cache
// memory database lives as long as a connection to it is open; Open keeps exactly one.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) index.Store {
		return openMigrated(t, fmt.Sprintf("file:storetest-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	})
}

func TestWithParams(t *testing.T) {
	tests := []struct {
		dsn, want string
		memory    bool
	}{
		{"file:lajstrom.db", "file:lajstrom.db?_busy_timeout=5000&_foreign_keys=1&_journal_mode=WAL&_synchronous=NORMAL&_txlock=immediate", false},
		{"/var/lib/lajstrom.db?_busy_timeout=100&_journal_mode=DELETE&_fk=0&_txlock=deferred",
			"/var/lib/lajstrom.db?_busy_timeout=100&_foreign_keys=1&_journal_mode=DELETE&_synchronous=NORMAL&_txlock=immediate", false},
		{"file:t?mode=memory&cache=shared", "file:t?_busy_timeout=5000&_foreign_keys=1&_journal_mode=WAL&_synchronous=NORMAL&_txlock=immediate&cache=shared&mode=memory", true},
		{":memory:", ":memory:?_busy_timeout=5000&_foreign_keys=1&_journal_mode=WAL&_synchronous=NORMAL&_txlock=immediate", true},
	}
	for _, tt := range tests {
		t.Run(tt.dsn, func(t *testing.T) {
			got, memory, err := withParams(tt.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want || memory != tt.memory {
				t.Errorf("withParams = %q, %v; want %q, %v", got, memory, tt.want, tt.memory)
			}
		})
	}
	if _, _, err := withParams("file:x?a=%zz"); err == nil {
		t.Error("withParams accepted a malformed query")
	}
}

// openFile opens dsn without migrating it and closes it when the test ends.
func openFile(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestFileMigrate migrates one new database file from two stores at once, as two replicas
// starting together do: BEGIN IMMEDIATE lets each migration apply once.
func TestFileMigrate(t *testing.T) {
	ctx := t.Context()
	dsn := "file:" + filepath.Join(t.TempDir(), "lajstrom.db")
	a, b := openFile(t, dsn), openFile(t, dsn)
	applied := make([][]int, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, s := range []*Store{a, b} {
		wg.Go(func() { applied[i], errs[i] = s.ApplyMigrations(ctx) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	want, err := index.MigrationVersions(index.DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(applied[0]) + len(applied[1]); n != len(want) {
		t.Errorf("concurrent migrations applied %v and %v, want %v once", applied[0], applied[1], want)
	}
}

// TestFileWriters checks a database file shared by two stores, as two replicas share it: each
// sees the other's writes, and a writer waits for the other's transaction (busy timeout)
// instead of failing, while a store without busy timeout fails.
func TestFileWriters(t *testing.T) {
	ctx := t.Context()
	dsn := "file:" + filepath.Join(t.TempDir(), "lajstrom.db")
	a := openMigrated(t, dsn)
	b, c := openFile(t, dsn), openFile(t, dsn+"?_busy_timeout=0")

	r := index.Repo{Format: "raw", Name: "shared", Registry: "local", Kind: "hosted"}
	if err := a.CreateRepo(ctx, &r); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetRepo(ctx, "raw", "shared"); err != nil {
		t.Errorf("the second store does not see the first's write: %v", err)
	}

	// a holds the write lock in a transaction while b writes and c tries to.
	locked := make(chan struct{})
	bDone := make(chan error, 1)
	go func() {
		<-locked
		bDone <- b.CreateRepo(context.WithoutCancel(ctx), &index.Repo{Format: "raw", Name: "waiter", Registry: "local", Kind: "hosted"})
	}()
	err := a.Tx(ctx, func(w index.Writer) error {
		if err := w.CreateRepo(ctx, &index.Repo{Format: "raw", Name: "holder", Registry: "local", Kind: "hosted"}); err != nil {
			return err
		}
		close(locked)
		if err := c.CreateRepo(ctx, &index.Repo{Format: "raw", Name: "impatient", Registry: "local", Kind: "hosted"}); err == nil {
			return errors.New("a writer without busy timeout succeeded while another held the lock")
		}
		select {
		case err := <-bDone:
			return fmt.Errorf("the second writer finished while the first held the lock: %w", err)
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-bDone; err != nil {
		t.Errorf("the second writer failed instead of waiting: %v", err)
	}
	for _, name := range []string{"holder", "waiter"} {
		if _, err := a.GetRepo(ctx, "raw", name); err != nil {
			t.Errorf("repo %s: %v", name, err)
		}
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	for _, dsn := range []string{
		"file:" + filepath.Join(dir, "missing", "x.db"),
		"file:" + filepath.Join(dir, "x.db") + "?_journal_mode=SIDEWAYS",
	} {
		if s, err := Open(t.Context(), dsn); err == nil {
			_ = s.Close()
			t.Errorf("Open(%q) succeeded", dsn)
		}
	}
}
