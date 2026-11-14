package ingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
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

// maxBlobRegistry is a registry whose capabilities say maxBlobSize.
type maxBlobRegistry struct {
	provider.Provider
	max int64
}

func (r maxBlobRegistry) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := r.Provider.Capabilities(ctx)
	c.MaxBlobSize = r.max
	return c, err
}

// TestIngestMaxBlobSize: the registry's maxBlobSize bounds a file when it is below the repo's
// maxFileSize (manifest-profile.md §5, #246), the message naming the registry; the frontends,
// which bound a body by the repo's maxFileSize alone, meet it here, before anything is pushed.
func TestIngestMaxBlobSize(t *testing.T) {
	e := newEnv(t)
	e.pipe = ingest.New(e.options(maxBlobRegistry{Provider: e.prov, max: 4}))
	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	_, err := e.ingest(t, built, raw, pkgPath)
	var mbe *http.MaxBytesError
	if !errors.Is(err, ingest.ErrInvalid) || !errors.As(err, &mbe) || mbe.Limit != 4 {
		t.Fatalf("Ingest of 5 bytes, maxBlobSize 4 = %v, want ErrInvalid and a MaxBytesError of 4", err)
	}
	if want := "the file Docs/Readme.md of 5 bytes is larger than the maxBlobSize of registry local, 4 bytes"; err.Error() != want {
		t.Errorf("message %q, want %q", err, want)
	}
	if len(e.prov.PushReferences()) != 0 {
		t.Errorf("the manifest was pushed: %+v", e.prov.PushReferences())
	}
	e.setPolicy(t, `{"maxFileSize":3}`)
	if _, err := e.ingest(t, built, raw, pkgPath); err == nil || !strings.Contains(err.Error(), "the maxFileSize of raw/scratch, 3 bytes") {
		t.Errorf("Ingest with maxFileSize 3 below maxBlobSize 4 = %v, want the repo's bound named", err)
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

// TestSessionFilesPolicy: a session stores the repo policy's maven.maxSessionFiles files; the
// next is ErrInvalid naming the bound and its key, and a file of the session may come again.
// Without the key, or with a value that does not parse, the bound is 256 (TestDeploySessionFiles
// of the maven frontend fills it).
func TestSessionFilesPolicy(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.setPolicy(t, `{"maven":{"maxSessionFiles":2}}`)
	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	add(t, e, s.ID, "a.jar", "aaa")
	add(t, e, s.ID, "b.jar", "bbb")
	_, err = e.pipe.Add(ctx, s.ID, "c.jar", strings.NewReader("c"), 1)
	if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), "holds 2 files, the bound policy.maven.maxSessionFiles") {
		t.Errorf("a third file = %v, want ErrInvalid naming the bound and its key", err)
	}
	add(t, e, s.ID, "a.jar", "again")
	e.setPolicy(t, `{"maven":{"maxSessionFiles":"many"}}`)
	add(t, e, s.ID, "c.jar", "ccc")
}

// admitPlugin is the raw plugin of the tests with an Admit that refuses the versions of refuse
// and records the entries it was asked about.
type admitPlugin struct {
	*rawPlugin
	refuse string
	asked  []format.Entry
}

func (p *admitPlugin) Admit(repo index.Repo, e format.Entry) error {
	p.asked = append(p.asked, e)
	if e.Version == p.refuse {
		return fmt.Errorf("%w: %s/%s takes no %s", format.ErrInvalid, repo.Format, repo.Name, e.Version)
	}
	return nil
}

// TestAdmit: a plugin that is a format.Admitter is asked about every publish, a manifest of /v2
// and a frontend's upload alike, after Validate, with its entry, and what it refuses is
// ErrInvalid before anything is claimed or pushed; Inspect, the path of reindex, never asks it
// (manifest-profile.md §5).
func TestAdmit(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	p := &admitPlugin{rawPlugin: e.plugin, refuse: "1.0.0"}
	opts := e.options(e.prov)
	opts.Plugins = func(name string) (format.Plugin, bool) { return p, name == "raw" }
	e.pipe = ingest.New(opts)

	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	_, err := e.ingest(t, built, raw, pkgPath)
	if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), "raw/scratch takes no 1.0.0") {
		t.Fatalf("Ingest of a version Admit refuses = %v, want ErrInvalid with its message", err)
	}
	if len(p.asked) != 1 || p.asked[0].Name != readme || p.asked[0].Version != "1.0.0" {
		t.Errorf("Admit was asked about %+v, want the entry of %s 1.0.0", p.asked, readme)
	}
	if _, err := e.pipe.Upload(ctx, e.repo, upload(readme, "1.0.0", file{readme, "hello"}), alice); !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Upload of a version Admit refuses = %v, want ErrInvalid", err)
	}
	if len(e.prov.PushReferences()) != 0 {
		t.Errorf("a refused version was pushed: %+v", e.prov.PushReferences())
	}
	e.noClaim(t, readme, built.Tag)

	asked := len(p.asked)
	if _, err := e.pipe.Inspect(ctx, e.repo, pkgPath, built.Tag, built.Manifest, raw, ingest.InspectOptions{}); err != nil {
		t.Errorf("Inspect of a version Admit refuses = %v, want the rows: reindex does not apply the policy", err)
	}
	if len(p.asked) != asked {
		t.Errorf("Inspect asked Admit: %+v", p.asked[asked:])
	}

	built, raw, pkgPath = e.prepare(t, upload(readme, "1.1.0", file{readme, "hello"}))
	if _, err := e.ingest(t, built, raw, pkgPath); err != nil {
		t.Errorf("Ingest of a version Admit takes = %v", err)
	}
}
