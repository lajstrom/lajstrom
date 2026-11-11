package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// serveConfig writes a configuration whose index database is a SQLite file in dir, served on a
// free loopback port and without the metrics listener, and returns its path and the server's
// address.
func serveConfig(t *testing.T, dir string) (cfg, addr string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = l.Addr().String()
	_ = l.Close()
	cfg = filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \"" + addr + "\"\n  metricsListen: \"\"\n  externalURL: https://art.example.com\n" +
		"database:\n  driver: sqlite\n  dsn: \"file:" + filepath.Join(dir, "lajstrom.db") + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, addr
}

// startServe runs serve with the configuration cfg, waits until it answers /-/healthz on addr,
// and returns stop, which ends it and returns its error. The test's end stops it too.
func startServe(t *testing.T, cfg, addr string) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		cmd := newRootCmd(func(string) (string, bool) { return "", false })
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"serve", "--config", cfg})
		done <- cmd.ExecuteContext(ctx)
	}()
	stopped := false
	stop = func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			return errors.New("serve has not returned 30 s after its context ended")
		}
	}
	t.Cleanup(func() { _ = stop() })
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("serve returned at start: %v", err)
		default:
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/-/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return stop
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve does not answer on %s after 20 s", addr)
		}
	}
}

// openIndex opens the SQLite index of the configuration serveConfig wrote in dir.
func openIndex(t *testing.T, dir string) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(t.Context(), "file:"+filepath.Join(dir, "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return st
}

// replicaIDs returns the IDs of the index's rows of replicas.
func replicaIDs(t *testing.T, st index.Store) []string {
	t.Helper()
	rows, err := st.ListReplicas(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// oneReplica returns the one row of replicas of the index, and stops the test when there is
// another number of rows.
func oneReplica(t *testing.T, st index.Store) index.Replica {
	t.Helper()
	rows, err := st.ListReplicas(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("replicas: %v, %v; want one", rows, err)
	}
	return rows[0]
}

// TestServeTwoReplicasOnSQLite checks docs/acceptance.md H-14: a second serve on the SQLite file
// of a live one exits with an error naming it, and the first one's stop deletes its row, so the
// next start need not wait.
func TestServeTwoReplicasOnSQLite(t *testing.T) {
	dir := t.TempDir()
	cfg, addr := serveConfig(t, dir)
	stopFirst := startServe(t, cfg, addr)
	st := openIndex(t, dir)
	first := oneReplica(t, st)

	_, _, err := run(t, nil, "serve", "--config", cfg)
	for _, want := range []string{"another replica serves on this SQLite database", first.ID, first.Host,
		"process " + strconv.Itoa(first.PID)} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("the second serve: %v, want an error with %q", err, want)
		}
	}
	if err := stopFirst(); err != nil {
		t.Fatalf("the first serve: %v", err)
	}
	if ids := replicaIDs(t, st); len(ids) != 0 {
		t.Errorf("replicas after the first serve stopped: %v, want none", ids)
	}
}

// TestServeAfterCrash checks a start on the SQLite file of a replica that stopped without
// deleting its row: refused while its heartbeat is fresh, and started once it is stale.
func TestServeAfterCrash(t *testing.T) {
	dir := t.TempDir()
	cfg, addr := serveConfig(t, dir)
	st := openIndex(t, dir)
	for _, tt := range []struct {
		name  string
		since time.Duration // since the crashed replica's last heartbeat
		start bool
	}{
		{"fresh heartbeat", 30 * time.Second, false},
		{"stale heartbeat", replicaStaleAfter + time.Second, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			crashed := index.Replica{ID: index.NewUID(now), Host: "crashed", PID: 1, Version: "v0.1.0",
				StartedAt: now.Add(-time.Hour), HeartbeatAt: now.Add(-tt.since)}
			if err := st.StartReplica(t.Context(), &crashed, now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if !tt.start {
				_, _, err := run(t, nil, "serve", "--config", cfg)
				if err == nil || !strings.Contains(err.Error(), crashed.ID) {
					t.Errorf("serve: %v, want an error naming %s", err, crashed.ID)
				}
				if err := st.StopReplica(t.Context(), crashed.ID); err != nil {
					t.Fatal(err)
				}
				return
			}
			stop := startServe(t, cfg, addr)
			if ids := replicaIDs(t, st); len(ids) != 1 || ids[0] == crashed.ID {
				t.Errorf("replicas: %v, want the row of the new serve only", ids)
			}
			if err := stop(); err != nil {
				t.Errorf("serve: %v", err)
			}
		})
	}
}

// TestReplicaBeat checks the heartbeat of a row: it moves HeartbeatAt; a row a start elsewhere
// deleted is written again; on SQLite, while another replica's row is fresh, Err says so without
// naming it and the log names it, and once that row is gone the next beat writes this one's
// again.
func TestReplicaBeat(t *testing.T) {
	ctx := t.Context()
	st := openIndex(t, t.TempDir())
	var logs bytes.Buffer
	now := time.Now()
	clock := func() time.Time { return now }
	r, err := startReplica(ctx, inSync{st, clock}, slog.New(slog.NewTextHandler(&logs, nil)), clock)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(replicaBeatEvery)
	r.beat(ctx)
	if row := oneReplica(t, st); !row.HeartbeatAt.Equal(now.Truncate(time.Microsecond).UTC()) || r.Err() != nil {
		t.Fatalf("after a beat: %v, %v; want HeartbeatAt %v", row, r.Err(), now)
	}

	if err := st.StopReplica(ctx, r.row.ID); err != nil {
		t.Fatal(err)
	}
	other := index.Replica{ID: "other", Host: "elsewhere", PID: 2, Version: "v0.1.0", StartedAt: now}
	if err := st.StartReplica(ctx, &other, now.Add(-replicaStaleAfter)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(replicaBeatEvery)
	r.beat(ctx)
	if err := r.Err(); err == nil || strings.Contains(err.Error(), "elsewhere") {
		t.Errorf("Err with another replica's fresh row: %v, want an error that names no host", err)
	}
	if !strings.Contains(logs.String(), "elsewhere") {
		t.Errorf("the log does not name the other replica:\n%s", logs.String())
	}

	if err := st.StopReplica(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(replicaBeatEvery)
	r.beat(ctx)
	if ids := replicaIDs(t, st); len(ids) != 1 || ids[0] != r.row.ID || r.Err() != nil {
		t.Errorf("after the other replica stopped: %v, %v; want this one's row again", ids, r.Err())
	}
	r.Stop(ctx)
	if ids := replicaIDs(t, st); len(ids) != 0 {
		t.Errorf("replicas after Stop: %v, want none", ids)
	}
}

// TestReplicaClock checks the comparison of this replica's clock with the database's past each
// bound, with a clock of this process that runs off by skew: within 10 s nothing is logged and
// the replica is ready; past it a warning; past 60 s an error, and readiness fails naming the
// skew, ahead or behind; back within the bounds it is ready again. lajstrom_clock_skew_seconds
// follows every measurement. The database is SQLite, whose clock is this host's.
func TestReplicaClock(t *testing.T) {
	ctx := t.Context()
	st := openIndex(t, t.TempDir())
	var logs bytes.Buffer
	var skew time.Duration
	r, err := startReplica(ctx, st, slog.New(slog.NewTextHandler(&logs, nil)), func() time.Time { return time.Now().Add(skew) })
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		skew     time.Duration
		level    string // of the line logged, "" for none
		readyErr string // "" when ready
	}{
		{0, "", ""},
		{15 * time.Second, "WARN", ""},
		{-15 * time.Second, "WARN", ""},
		{75 * time.Second, "ERROR", "the clock of this replica is 1m15s ahead of the database's"},
		{-75 * time.Second, "ERROR", "the clock of this replica is 1m15s behind the database's"},
		{2 * time.Second, "", ""},
	} {
		logs.Reset()
		skew = tt.skew
		r.beat(ctx)
		got := testutil.ToFloat64(metrics.ClockSkew)
		if d := got - tt.skew.Seconds(); d < -1 || d > 1 {
			t.Errorf("skew %s: lajstrom_clock_skew_seconds = %v", tt.skew, got)
		}
		if level := logLevel(logs.String()); level != tt.level {
			t.Errorf("skew %s: logged %q, want a line of level %q:\n%s", tt.skew, level, tt.level, logs.String())
		}
		switch err := r.Err(); {
		case tt.readyErr == "" && err != nil:
			t.Errorf("skew %s: not ready: %v", tt.skew, err)
		case tt.readyErr != "" && (err == nil || err.Error() != tt.readyErr):
			t.Errorf("skew %s: readiness %v, want %q", tt.skew, err, tt.readyErr)
		}
	}
}

// logLevel returns the level of the first line of a text log, "" for none.
func logLevel(log string) string {
	_, rest, ok := strings.Cut(log, "level=")
	if !ok {
		return ""
	}
	level, _, _ := strings.Cut(rest, " ")
	return level
}

// inSync is an index whose clock is now, the clock of the replica under test.
type inSync struct {
	index.Store
	now func() time.Time
}

// DatabaseTime returns now().
func (s inSync) DatabaseTime(context.Context) (time.Time, error) { return s.now(), nil }
