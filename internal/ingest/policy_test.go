package ingest_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

// setPolicy stores policy as the policy of the env's repo, in the index and in e.repo.
func (e *env) setPolicy(t *testing.T, policy string) {
	t.Helper()
	e.repo.Policy = json.RawMessage(policy)
	must(t, e.st.UpdateRepo(t.Context(), &e.repo))
}

// TestIngestMaxFileSize: a file above the repo's maxFileSize is refused before anything is
// claimed or pushed, as ErrInvalid with the limit in an *http.MaxBytesError; a file of exactly
// the limit passes. Inspect, the path of reindex, keeps what the registry has whatever the
// policy.
func TestIngestMaxFileSize(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	e.setPolicy(t, `{"maxFileSize":4}`)

	_, err := e.ingest(t, built, raw, pkgPath)
	var mbe *http.MaxBytesError
	if !errors.Is(err, ingest.ErrInvalid) || !errors.As(err, &mbe) || mbe.Limit != 4 {
		t.Fatalf("Ingest of 5 bytes with maxFileSize 4 = %v, want ErrInvalid and a MaxBytesError of 4", err)
	}
	if want := "the file Docs/Readme.md of 5 bytes is larger than the maxFileSize of raw/scratch, 4 bytes"; err.Error() != want {
		t.Errorf("message %q, want %q", err, want)
	}
	if len(e.prov.PushReferences()) != 0 {
		t.Errorf("the manifest was pushed: %+v", e.prov.PushReferences())
	}
	e.noClaim(t, readme, built.Tag)

	if _, err := e.pipe.Inspect(ctx, e.repo, pkgPath, built.Tag, built.Manifest, raw, ingest.InspectOptions{}); err != nil {
		t.Errorf("Inspect with maxFileSize 4 = %v, want the rows: reindex does not apply the policy", err)
	}

	e.setPolicy(t, `{"maxFileSize":5}`)
	if _, err := e.ingest(t, built, raw, pkgPath); err != nil {
		t.Errorf("Ingest of 5 bytes with maxFileSize 5 = %v", err)
	}
}

// TestSessionTimeoutPolicy: a session lives the repo policy's maven.sessionTimeout after its
// last request, and Options.SessionTimeout, 30 minutes when zero, without one. ingest reads the
// key whatever the format; the API allows it on maven repos only.
func TestSessionTimeoutPolicy(t *testing.T) {
	ctx := t.Context()
	for _, tt := range []struct {
		name, policy string
		timeout      time.Duration
	}{
		{"the policy's", `{"maven":{"sessionTimeout":"2m"}}`, 2 * time.Minute},
		{"the default", ``, 30 * time.Minute},
		{"the default for a policy without the key", `{"maxFileSize":10}`, 30 * time.Minute},
		{"the default for a value that does not parse", `{"maven":{"sessionTimeout":"soon"}}`, 30 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			c := &clock{t: now}
			o := e.options(e.prov)
			o.Now = c.Now
			e.pipe = ingest.New(o)
			if tt.policy != "" {
				e.setPolicy(t, tt.policy)
			}
			s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
			must(t, err)
			if got := s.ExpiresAt.Sub(now); got != tt.timeout {
				t.Errorf("Open: the session expires after %v, want %v", got, tt.timeout)
			}
			c.add(tt.timeout - time.Second)
			add(t, e, s.ID, "a.jar", "aaa") // moves the expiry on by the timeout
			c.add(tt.timeout - time.Second)
			add(t, e, s.ID, "b.jar", "bbb")
			c.add(tt.timeout + time.Second)
			if _, err := e.pipe.Add(ctx, s.ID, "c.jar", strings.NewReader("c"), 1); !errors.Is(err, index.ErrNotFound) {
				t.Errorf("Add after the timeout = %v, want ErrNotFound", err)
			}
		})
	}
}
