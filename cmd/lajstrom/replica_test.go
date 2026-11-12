package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// serveConfig writes a configuration whose index database is a SQLite file in dir, served on a
// free loopback port, without the metrics listener and without the shutdown delay, and returns
// its path and the server's address.
func serveConfig(t *testing.T, dir string) (cfg, addr string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = l.Addr().String()
	_ = l.Close()
	cfg = filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \"" + addr + "\"\n  metricsListen: \"\"\n  shutdownDelay: 0s\n  externalURL: https://art.example.com\n" +
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
	awaitHealthz(t, addr, func() error {
		select {
		case err := <-done:
			return fmt.Errorf("serve returned at start: %w", err)
		default:
			return nil
		}
	})
	return stop
}

// awaitHealthz waits until a serve answers /-/healthz on addr, and stops the test when it does
// not within 20 s, or when ended, asked between the tries, says that serve has ended.
func awaitHealthz(t *testing.T, addr string, ended func() error) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if err := ended(); err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/-/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve does not answer on %s after 20 s", addr)
		}
	}
}

// envServeProcess names the configuration of serve in the process of startServeProcess.
const envServeProcess = "LAJSTROM_TEST_SERVE_CONFIG"

// TestServeProcess is serve in the child process of startServeProcess, with the configuration
// of LAJSTROM_TEST_SERVE_CONFIG; without it, it is skipped.
func TestServeProcess(t *testing.T) {
	cfg := os.Getenv(envServeProcess)
	if cfg == "" {
		t.Skip("the child process of startServeProcess")
	}
	cmd := newRootCmd(func(string) (string, bool) { return "", false })
	cmd.SetArgs([]string{"serve", "--config", cfg})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// startServeProcess runs serve with the configuration cfg in a child process of its own
// (TestServeProcess), so that the test can signal it, and waits until it answers /-/healthz on
// addr. wait returns the end of the process (exec.Cmd.Wait) with its output; the test's end
// kills it.
func startServeProcess(t *testing.T, cfg, addr string) (p *os.Process, wait func() error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeProcess$") //nolint:gosec // the test binary
	cmd.Env = append(os.Environ(), envServeProcess+"="+cfg)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var werr error
	go func() { werr = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	wait = func() error {
		select {
		case <-exited:
			if werr != nil {
				return fmt.Errorf("%w; its output:\n%s", werr, out.String())
			}
			return nil
		case <-time.After(30 * time.Second):
			return errors.New("serve has not ended 30 s after the signal")
		}
	}
	awaitHealthz(t, addr, func() error {
		select {
		case <-exited:
			return fmt.Errorf("serve ended at start: %w; its output:\n%s", werr, out.String())
		default:
			return nil
		}
	})
	return cmd.Process, wait
}

// deadPID returns the PID of a process of this host that has ended: a child that ran no test
// and was waited for.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // the test binary
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
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

// TestServeHangup checks that serve drains on SIGHUP, which closing its terminal or an SSH
// session sends, as it does on SIGTERM: it exits 0 and deletes its row, so the next start need
// not wait (#720).
func TestServeHangup(t *testing.T) {
	dir := t.TempDir()
	cfg, addr := serveConfig(t, dir)
	p, wait := startServeProcess(t, cfg, addr)
	st := openIndex(t, dir)
	if row := oneReplica(t, st); row.PID != p.Pid {
		t.Fatalf("the row of serve: %+v, want PID %d", row, p.Pid)
	}
	if err := p.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if err := wait(); err != nil {
		t.Fatalf("serve after SIGHUP: %v, want exit status 0", err)
	}
	if ids := replicaIDs(t, st); len(ids) != 0 {
		t.Errorf("replicas after SIGHUP: %v, want none", ids)
	}
}

// TestServeKilled checks a start on the SQLite file of a serve of this host that was killed
// (SIGKILL, as by the OOM killer) and left its row with a fresh heartbeat: the row's process
// is gone, so the start deletes the row and serves at once (#720).
func TestServeKilled(t *testing.T) {
	if processScope() == "" {
		t.Skip("this operating system does not tell Lajstrom whether a process is gone")
	}
	dir := t.TempDir()
	cfg, addr := serveConfig(t, dir)
	p, wait := startServeProcess(t, cfg, addr)
	st := openIndex(t, dir)
	killed := oneReplica(t, st)
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := wait(); err == nil {
		t.Fatal("serve after SIGKILL: exit status 0")
	}
	if row := oneReplica(t, st); row.ID != killed.ID {
		t.Fatalf("replicas after SIGKILL: %+v, want the killed one's row", row)
	}
	stop := startServe(t, cfg, addr)
	if ids := replicaIDs(t, st); len(ids) != 1 || ids[0] == killed.ID {
		t.Errorf("replicas: %v, want the row of the new serve only", ids)
	}
	if err := stop(); err != nil {
		t.Errorf("serve: %v", err)
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

// TestReplicaBeatGone checks a beat that finds the row of this replica deleted and a fresh row
// of a process of this host that is gone: it deletes that row and writes this one's again.
func TestReplicaBeatGone(t *testing.T) {
	if processScope() == "" {
		t.Skip("this operating system does not tell Lajstrom whether a process is gone")
	}
	ctx := t.Context()
	st := openIndex(t, t.TempDir())
	now := time.Now()
	clock := func() time.Time { return now }
	r, err := startReplica(ctx, inSync{st, clock}, slog.New(slog.DiscardHandler), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StopReplica(ctx, r.row.ID); err != nil {
		t.Fatal(err)
	}
	gone := index.Replica{ID: "gone", Host: r.row.Host, PID: deadPID(t), Process: r.row.Process, Version: "v0.1.0", StartedAt: now}
	if err := st.StartReplica(ctx, &gone, now.Add(-replicaStaleAfter)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(replicaBeatEvery)
	r.beat(ctx)
	if ids := replicaIDs(t, st); len(ids) != 1 || ids[0] != r.row.ID || r.Err() != nil {
		t.Errorf("after the beat: %v, %v; want this one's row only", ids, r.Err())
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
