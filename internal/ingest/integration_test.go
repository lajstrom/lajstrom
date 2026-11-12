//go:build integration

package ingest_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// TestIntegration runs Upload with the raw-like plugin and the generic provider against the
// registry at LAJSTROM_TEST_REGISTRY (host:port, plain HTTP), with the instance prefix of
// LAJSTROM_TEST_REGISTRY_PREFIX and the credentials of LAJSTROM_TEST_REGISTRY_USERNAME and
// _PASSWORD when they are set: two files give one manifest with two file layers and the
// metadata layer, the index rows follow, the same upload again is idempotent and other bytes
// under the tag are ErrConflict. The index is SQLite in memory; the repo has a random name, so
// runs do not collide in a registry that keeps what they pushed.
func TestIntegration(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, prov := integrationEnv(t, addr)
	up := upload("tool", "1.0.0", file{"tool-linux", "linux binary " + e.repo.Name}, file{"tool-darwin", "darwin binary " + e.repo.Name})
	want, err := e.plugin.Build(ctx, e.repo, up)
	must(t, err)
	wantRaw, wantDigest, err := profile.Encode(want.Manifest)
	must(t, err)
	name := "raw/" + e.repo.Name + "/tool"

	pub, err := e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	same(t, "Published digest, tag and path", []string{wantDigest.String(), "1.0.0", name},
		[]string{pub.Digest.String(), pub.Tag, pub.RepoPath})
	rr, err := prov.Repository(ctx, name)
	must(t, err)
	stored, m := fetchManifest(ctx, t, rr, "1.0.0")
	if string(stored) != string(wantRaw) {
		t.Errorf("manifest in the registry:\n%s\nwant\n%s", stored, wantRaw)
	}
	checkManifest(ctx, t, e, rr, m)
	same(t, "index rows", rowSummary{Versions: []digest.Digest{wantDigest}, Files: []int{2}, OCIRepos: []string{name}, Audit: 1},
		summarise(e.rows(t)))
	e.noClaim(t, "tool", "1.0.0")

	again, err := e.pipe.Upload(ctx, e.repo, up, alice)
	if err != nil || again.Digest != wantDigest {
		t.Errorf("second identical Upload: %+v, %v", again, err)
	}
	res, err := e.pipe.Ingest(ctx, e.repo, "tool", "1.0.0", m, stored, alice)
	if err != nil || !res.Existed {
		t.Errorf("Ingest of the stored manifest: %+v, %v; want Existed", res, err)
	}
	if n := len(e.rows(t).Audit); n != 1 {
		t.Errorf("%d audit events after the repeats, want 1", n)
	}

	_, err = e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool-linux", "rebuilt " + e.repo.Name}), alice)
	wantTagConflict(t, err, wantDigest)
	if d := tagDigest(ctx, rr, "1.0.0"); d != wantDigest {
		t.Errorf("tag after the conflict: %s, want %s", d, wantDigest)
	}
}

// TestIntegrationRetryUnconfirmed runs TestUploadRetryUnconfirmed's first case against the
// registry of TestIntegration: an upload whose confirm finds the database out is 503, other files
// under the tag are then ErrConflict, and the upload again, built a minute later, confirms the
// manifest the tag holds, which it keeps.
func TestIntegrationRetryUnconfirmed(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, prov := integrationEnv(t, addr)
	o := e.options(prov)
	o.Index = &confirmDown{Store: e.st}
	e.pipe = ingest.New(o)
	up := upload("tool", "1.0.0", file{"tool", "linux binary " + e.repo.Name})
	if _, err := e.pipe.Upload(ctx, e.repo, up, alice); !errors.Is(err, index.ErrUnavailable) {
		t.Fatalf("Upload while the index is out: %v, want index.ErrUnavailable", err)
	}
	rr, err := prov.Repository(ctx, "raw/"+e.repo.Name+"/tool")
	must(t, err)
	first := tagDigest(ctx, rr, "1.0.0")
	_, err = e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool", "other bytes " + e.repo.Name}), alice)
	wantTagConflict(t, err, first)

	e.plugin.BuildFunc = buildRawAt(created.Add(time.Minute))
	pub, err := e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	if pub.Digest != first || pub.Existed || tagDigest(ctx, rr, "1.0.0") != first {
		t.Errorf("the retry: %s, Existed %v, the tag on %s; want the first manifest, %s, confirmed now",
			pub.Digest, pub.Existed, tagDigest(ctx, rr, "1.0.0"), first)
	}
	if r := e.rows(t); len(r.Versions) != 1 || r.Versions[0].Digest != first {
		t.Errorf("versions %+v, want the first manifest's", r.Versions)
	}
}

// integrationEnv returns an env whose pipeline uses the generic provider for addr, and a repo
// with a random name.
func integrationEnv(t *testing.T, addr string) (*env, provider.Provider) {
	t.Helper()
	prov, err := generic.New(config.Registry{
		Name:   "local",
		Type:   generic.Type,
		URL:    "http://" + addr,
		Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"),
		Auth: config.RegistryAuth{
			Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"),
			Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD"),
		},
	}, slog.New(slog.DiscardHandler))
	must(t, err)
	e := newEnv(t)
	run := make([]byte, 4)
	_, _ = rand.Read(run)
	e.repo = index.Repo{Format: "raw", Name: "it-" + hex.EncodeToString(run), Registry: "local", Kind: "hosted"}
	must(t, e.st.CreateRepo(t.Context(), &e.repo))
	e.pipe = ingest.New(e.options(prov))
	return e, prov
}

// fetchManifest returns the manifest under tag, as bytes and parsed.
func fetchManifest(ctx context.Context, t *testing.T, rr provider.Repository, tag string) ([]byte, ocispec.Manifest) {
	t.Helper()
	desc, err := rr.Resolve(ctx, tag)
	must(t, err)
	rc, err := rr.Fetch(ctx, desc)
	must(t, err)
	defer func() { _ = rc.Close() }()
	stored, err := io.ReadAll(rc)
	must(t, err)
	var m ocispec.Manifest
	must(t, json.Unmarshal(stored, &m))
	return stored, m
}

// checkManifest checks the annotations and layers of the uploaded manifest, and that its
// layers are in the registry.
func checkManifest(ctx context.Context, t *testing.T, e *env, rr provider.Repository, m ocispec.Manifest) {
	t.Helper()
	parsed, err := profile.Validate(m)
	must(t, err)
	type summary struct {
		Format, Repo, Name, Version, ArtifactType string
		Created                                   time.Time
		Config                                    digest.Digest
		Files                                     []string
		Metadata                                  bool
	}
	a := parsed.Annotations
	got := summary{a.Format, a.Repo, a.Name, a.Version, m.ArtifactType, a.Created, m.Config.Digest, nil, parsed.Metadata != nil}
	for _, f := range parsed.Files {
		got.Files = append(got.Files, f.Title)
	}
	same(t, "manifest", summary{"raw", e.repo.Name, "tool", "1.0.0", "application/vnd.lajstrom.raw.v1", created,
		profile.EmptyConfig.Digest, []string{"tool-linux", "tool-darwin"}, true}, got)
	for _, l := range m.Layers {
		if ok, err := rr.Exists(ctx, l); !ok || err != nil {
			t.Errorf("layer %s in the registry: %v, %v", l.Digest, ok, err)
		}
	}
}

// rowSummary is what the integration test checks of the index rows.
type rowSummary struct {
	Versions []digest.Digest // digests of the versions with metadata
	Files    []int           // files per version
	OCIRepos []string
	Audit    int
}

func summarise(r rows) rowSummary {
	var s rowSummary
	for i, v := range r.Versions {
		if v.Metadata != nil {
			s.Versions = append(s.Versions, v.Digest)
		}
		s.Files = append(s.Files, len(r.Files[i]))
	}
	for _, o := range r.OCIRepos {
		s.OCIRepos = append(s.OCIRepos, o.Path)
	}
	s.Audit = len(r.Audit)
	return s
}

// tagDigest returns the digest under tag, or "" when it does not resolve.
func tagDigest(ctx context.Context, rr provider.Repository, tag string) digest.Digest {
	desc, err := rr.Resolve(ctx, tag)
	if err != nil {
		return ""
	}
	return desc.Digest
}

// TestSessionsIntegration runs publication sessions against the registry of TestIntegration:
// five files added one by one commit as one manifest with five file layers and the metadata
// layer, and one version of five files in the index; an aborted session and one whose time is up
// publish nothing.
func TestSessionsIntegration(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, prov := integrationEnv(t, addr)
	c := &clock{t: now}
	o := e.options(prov)
	o.Now, o.SessionTimeout = c.Now, time.Minute
	e.pipe = ingest.New(o)

	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	for i := range 5 {
		add(t, e, s.ID, fmt.Sprintf("file-%d", i), fmt.Sprintf("file %d of %s", i, e.repo.Name))
	}
	pub, err := e.pipe.Commit(ctx, s.ID)
	must(t, err)
	rr, err := prov.Repository(ctx, "raw/"+e.repo.Name+"/lib")
	must(t, err)
	_, m := fetchManifest(ctx, t, rr, pub.Tag)
	if len(m.Layers) != 6 {
		t.Errorf("the manifest has %d layers, want five files and the metadata layer", len(m.Layers))
	}
	for i := range 5 {
		if want := digest.FromString(fmt.Sprintf("file %d of %s", i, e.repo.Name)); m.Layers[i].Digest != want {
			t.Errorf("layer %d: %s, want %s", i, m.Layers[i].Digest, want)
		}
	}

	aborted, err := e.pipe.Open(ctx, e.repo, "aborted", "1.0", alice)
	must(t, err)
	add(t, e, aborted.ID, "file", "aborted "+e.repo.Name)
	must(t, e.pipe.Abort(ctx, aborted.ID))
	late, err := e.pipe.Open(ctx, e.repo, "late", "1.0", alice)
	must(t, err)
	add(t, e, late.ID, "file", "late "+e.repo.Name)
	c.add(2 * time.Minute)
	if _, err := e.pipe.Commit(ctx, late.ID); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Commit after the timeout = %v, want ErrNotFound", err)
	}
	if n, err := e.pipe.ExpireSessions(ctx); err != nil || n != 1 {
		t.Errorf("ExpireSessions = %d, %v; want the late session", n, err)
	}
	r := e.rows(t)
	if len(r.Versions) != 1 || len(r.Files[0]) != 5 || r.Packages[0].Name != "lib" {
		t.Errorf("index: %+v; want the five files of lib 1.0 alone", r)
	}
}
