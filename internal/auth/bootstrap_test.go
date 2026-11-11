package auth

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// bootstrapClock is a clock a test moves.
type bootstrapClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *bootstrapClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *bootstrapClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newBootstrap returns n replicas of the bootstrap over one index, each logging JSON into a
// buffer of its own, and the clock they share.
func newBootstrap(t *testing.T, n int) ([]*Bootstrap, []*bytes.Buffer, *Users, *bootstrapClock) {
	t.Helper()
	users, _, _ := newUsers(t)
	clock := &bootstrapClock{t: usersNow}
	var replicas []*Bootstrap
	var logs []*bytes.Buffer
	for i := range n {
		var buf bytes.Buffer
		logs = append(logs, &buf)
		replicas = append(replicas, &Bootstrap{Index: users.Index, Users: users, TTL: time.Hour, Replica: fmt.Sprintf("r%d:1", i),
			Now: clock.now, Log: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	}
	return replicas, logs, users, clock
}

// writeToken writes a token file of the test and returns its path.
func writeToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// noSecret fails the test when a log of logs holds secret.
func noSecret(t *testing.T, secret string, logs ...*bytes.Buffer) {
	t.Helper()
	for i, l := range logs {
		if strings.Contains(l.String(), secret) {
			t.Errorf("log %d holds the token: %s", i, l)
		}
	}
}

// arm arms b with a file of token.
func arm(t *testing.T, b *Bootstrap, token string) {
	t.Helper()
	if err := b.Arm(t.Context(), writeToken(t, token)); err != nil {
		t.Fatal(err)
	}
}

// TestBootstrapNoToken: without a file, or with a file that does not exist, no token is made,
// the warning says how to make the first admin, and no exchange succeeds.
func TestBootstrapNoToken(t *testing.T) {
	ctx := t.Context()
	bs, logs, users, _ := newBootstrap(t, 1)
	gone := filepath.Join(t.TempDir(), "gone")
	for _, file := range []string{"", gone} {
		logs[0].Reset()
		if err := bs[0].Arm(ctx, file); err != nil {
			t.Fatalf("Arm(%q): %v", file, err)
		}
		for _, want := range []string{`"level":"WARN"`, "no bootstrap token", "openssl rand -hex 32", "lajstrom admin user create --admin"} {
			if !strings.Contains(logs[0].String(), want) {
				t.Errorf("Arm(%q) logged %s, want %q", file, logs[0], want)
			}
		}
	}
	if !strings.Contains(logs[0].String(), gone+" does not exist") {
		t.Errorf("the warning does not name the missing file: %s", logs[0])
	}
	if _, err := users.Index.GetBootstrapToken(ctx); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("GetBootstrapToken: %v, want no token", err)
	}
	if _, err := bs[0].Exchange(ctx, strings.Repeat("a", 40), "root", "root-password", Origin{}); !errors.Is(err, ErrBootstrapToken) {
		t.Errorf("Exchange without a token: %v, want ErrBootstrapToken", err)
	}
}

// TestBootstrapTokenFile: the token of the file is made valid, its hash stored and nothing of
// it logged; it makes one admin, after which the exchange is closed whatever the token; a start
// while the file exists warns to remove it, and once it is gone does not.
func TestBootstrapTokenFile(t *testing.T) {
	ctx := t.Context()
	bs, logs, users, _ := newBootstrap(t, 1)
	b := bs[0]
	secret := strings.Repeat("0f", 32)
	arm(t, b, secret)
	row, err := users.Index.GetBootstrapToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row.Hash != HashSecret(secret) || strings.Contains(fmt.Sprint(*row), secret) {
		t.Errorf("stored %+v, want the hash of the token only", *row)
	}
	if !row.ExpiresAt.Equal(usersNow.Add(time.Hour)) || row.Replica != "r0:1" {
		t.Errorf("stored expiry %v by %s, want %v by r0:1", row.ExpiresAt, row.Replica, usersNow.Add(time.Hour))
	}

	u, err := b.Exchange(ctx, secret, "root", "root-password", Origin{Source: "bootstrap", IP: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "root" {
		t.Errorf("Exchange made %+v", u)
	}
	wantAudit(t, users.Index, `anonymous user.create user:root {"admin":true,"name":"root","source":"bootstrap"}`)
	for _, tok := range []string{secret, "wrong"} {
		if _, err := b.Exchange(ctx, tok, "root2", "root-password", Origin{}); !errors.Is(err, ErrBootstrapClosed) {
			t.Errorf("Exchange of %q with an admin: %v, want ErrBootstrapClosed", tok, err)
		}
	}

	arm(t, b, "not even read")
	if !strings.Contains(logs[0].String(), "no longer valid") {
		t.Errorf("a start with an admin and the file logged %s", logs[0])
	}
	logs[0].Reset()
	if err := b.Arm(ctx, filepath.Join(t.TempDir(), "gone")); err != nil {
		t.Fatalf("Arm with an admin and a file that is gone: %v", err)
	}
	if logs[0].Len() != 0 {
		t.Errorf("a start with an admin and the file gone logged %s", logs[0])
	}
	noSecret(t, secret, logs...)
}

// TestBootstrapReplicas: replicas that start together with one file make one token; replicas
// that exchange it together make one admin.
func TestBootstrapReplicas(t *testing.T) {
	ctx := t.Context()
	const n = 8
	bs, logs, users, _ := newBootstrap(t, n)
	secret := strings.Repeat("r", 48)
	file := writeToken(t, secret)
	var wg sync.WaitGroup
	for _, b := range bs {
		wg.Go(func() {
			if err := b.Arm(ctx, file); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	row, err := users.Index.GetBootstrapToken(ctx)
	if err != nil || row.Hash != HashSecret(secret) {
		t.Fatalf("stored %+v, %v, want the file's hash", row, err)
	}
	made := 0
	for _, l := range logs {
		if strings.Contains(l.String(), `"madeBy":"`+row.Replica+`"`) {
			made++
		}
	}
	if made != n {
		t.Errorf("%d replicas logged that %s made the token, want %d", made, row.Replica, n)
	}

	admins := exchangeAll(t, bs, secret)
	list, err := users.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if admins != 1 || len(list) != 1 {
		t.Errorf("%d exchanges made an admin and %d users exist, want 1 and 1", admins, len(list))
	}
	noSecret(t, secret, logs...)
}

// exchangeAll exchanges secret on every replica of bs at once, each for a name of its own, and
// returns how many made an admin; the others must be refused or closed.
func exchangeAll(t *testing.T, bs []*Bootstrap, secret string) int {
	t.Helper()
	errs := make([]error, len(bs))
	var wg sync.WaitGroup
	for i, b := range bs {
		wg.Go(func() {
			_, errs[i] = b.Exchange(t.Context(), secret, fmt.Sprintf("admin%d", i), "admin-password", Origin{})
		})
	}
	wg.Wait()
	admins := 0
	for i, err := range errs {
		switch {
		case err == nil:
			admins++
		case !errors.Is(err, ErrBootstrapToken) && !errors.Is(err, ErrBootstrapClosed):
			t.Errorf("replica %d: %v, want ErrBootstrapToken or ErrBootstrapClosed", i, err)
		}
	}
	return admins
}

// TestBootstrapExpiry: an expired token is refused with a message that says so; a start with
// the same file does not make it valid again and warns, a start with a new token in the file
// replaces it, and the old one stays dead.
func TestBootstrapExpiry(t *testing.T) {
	ctx := t.Context()
	bs, logs, _, clock := newBootstrap(t, 2)
	old := strings.Repeat("o", 40)
	arm(t, bs[0], old)
	clock.add(time.Hour)
	_, err := bs[1].Exchange(ctx, old, "root", "root-password", Origin{})
	if !errors.Is(err, ErrBootstrapToken) || !strings.Contains(err.Error(), "expired at") {
		t.Fatalf("Exchange of an expired token: %v, want ErrBootstrapToken that it expired", err)
	}
	arm(t, bs[1], old)
	if !strings.Contains(logs[1].String(), "used or expired") {
		t.Errorf("a start with the expired token of the file logged %s", logs[1])
	}
	if _, err := bs[0].Exchange(ctx, old, "root", "root-password", Origin{}); !errors.Is(err, ErrBootstrapToken) {
		t.Errorf("Exchange after a start with the expired token: %v, want ErrBootstrapToken", err)
	}
	next := strings.Repeat("n", 40)
	arm(t, bs[1], next)
	if _, err := bs[0].Exchange(ctx, old, "root", "root-password", Origin{}); !errors.Is(err, ErrBootstrapToken) {
		t.Errorf("Exchange of the replaced token: %v, want ErrBootstrapToken", err)
	}
	if _, err := bs[0].Exchange(ctx, next, "root", "root-password", Origin{}); err != nil {
		t.Errorf("Exchange of the new token: %v", err)
	}
	noSecret(t, old, logs...)
	noSecret(t, next, logs...)
}

// TestBootstrapRefusals: a wrong token is refused and counts against the address, whose
// BootstrapFailures-th failure throttles it, the right token too; another address is not; a
// bad name or password, or a name that exists, leaves the token valid.
func TestBootstrapRefusals(t *testing.T) {
	ctx := t.Context()
	bs, logs, users, clock := newBootstrap(t, 1)
	b := bs[0]
	secret := strings.Repeat("s", 40)
	arm(t, b, secret)
	if _, err := users.Create(ctx, "alice", "alice-password", false, cli); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, user, password string
		want                 error
	}{
		{"a bad name", "Root", "root-password", ErrInvalid},
		{"a short password", "root", "short", ErrInvalid},
		{"a name that exists", "alice", "alice-password", index.ErrConflict},
	} {
		if _, err := b.Exchange(ctx, secret, tt.user, tt.password, Origin{}); !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
	}

	attacker := WithClientAddress(ctx, "203.0.113.9")
	for i := range BootstrapFailures {
		if _, err := b.Exchange(attacker, secret+"x", "root", "root-password", Origin{}); !errors.Is(err, ErrBootstrapToken) {
			t.Fatalf("wrong token %d: %v, want ErrBootstrapToken", i, err)
		}
	}
	var te *ThrottledError
	if _, err := b.Exchange(attacker, secret, "root", "root-password", Origin{}); !errors.As(err, &te) || te.Throttle != "bootstrap" {
		t.Fatalf("the right token from a throttled address: %v, want the throttle bootstrap", err)
	}
	clock.add(LoginWindow)
	if _, err := b.Exchange(WithClientAddress(ctx, "198.51.100.7"), secret, "root", "root-password", Origin{}); err != nil {
		t.Errorf("the right token from another address: %v", err)
	}
	noSecret(t, secret, logs...)
}

func TestReadBootstrapToken(t *testing.T) {
	for _, tt := range []struct {
		name, content, want string
	}{
		{"64 hexadecimal digits", strings.Repeat("0f", 32), ""},
		{"too short", strings.Repeat("a", MinBootstrapTokenLen-1), "must be 32 to 256 bytes, it has 31"},
		{"too long", strings.Repeat("a", MaxBootstrapTokenLen+1), "must be 32 to 256 bytes, it has 257"},
		{"two words", strings.Repeat("a", 20) + " " + strings.Repeat("b", 20), "without whitespace"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadBootstrapToken(writeToken(t, tt.content))
			switch {
			case tt.want == "" && (err != nil || got != tt.content):
				t.Errorf("ReadBootstrapToken = %q, %v, want the content", got, err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("ReadBootstrapToken: %v, want %q", err, tt.want)
			case err != nil && strings.Contains(err.Error(), tt.content):
				t.Errorf("the error has the token: %v", err)
			}
		})
	}
	if _, err := ReadBootstrapToken(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("ReadBootstrapToken of no file: nil error")
	}
}
