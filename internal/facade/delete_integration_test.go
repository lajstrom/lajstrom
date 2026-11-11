//go:build integration

package facade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
)

// deleteSetup is a facade over raw/assets on the registry at addr, whose provider has the
// capability overrides caps, and a version pushed through it with what refers to it.
type deleteSetup struct {
	st      *sqlite.Store
	logs    *logBuffer
	name    string // <srv>/v2/raw/assets/<path>
	f       *rawFile
	subject digest.Digest
	refs    []digest.Digest // the signature, the attestation, cosign's legacy signature
}

func newDeleteSetup(t *testing.T, addr string, caps config.CapabilityOverrides) *deleteSetup {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:facade-it-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: "raw", Name: "assets", Registry: "it", Prefix: "raw/assets", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &repo))
	s := &deleteSetup{st: st, logs: &logBuffer{}}
	log := slog.New(slog.NewTextHandler(s.logs, nil))
	p, err := generic.New(config.Registry{Name: "it", Type: generic.Type, URL: "http://" + addr, Capabilities: caps,
		Prefix: os.Getenv("LAJSTROM_TEST_REGISTRY_PREFIX"), Auth: config.RegistryAuth{Username: os.Getenv("LAJSTROM_TEST_REGISTRY_USERNAME"),
			Password: os.Getenv("LAJSTROM_TEST_REGISTRY_PASSWORD")}}, log)
	must(t, err)
	providers := provider.Map{"it": p}
	srv := httptest.NewServer(New(Options{Index: st, Providers: providers, Log: log, Tokens: keys.NewEphemeralTokens(),
		Ingest: ingest.New(ingest.Options{Providers: providers, Index: st, Log: log})}))
	t.Cleanup(srv.Close)
	s.f = rawFileOf("it-"+randomHex(t)+"/delete.txt", "delete me "+randomHex(t)+"\n").at(time.Now())
	s.name = srv.URL + "/v2/raw/assets/" + s.f.path
	for _, b := range [][]byte{s.f.content, ocispec.DescriptorEmptyJSON.Data, []byte("signature"), []byte("attestation")} {
		s.expect(t, "blob", http.MethodPost, "/blobs/uploads/?digest="+digest.FromBytes(b).String(), b, http.StatusCreated)
	}
	m, d := s.f.body(t)
	s.subject = d
	s.expect(t, "the version", http.MethodPut, "/manifests/"+s.f.tag(), m, http.StatusCreated)
	for _, r := range []struct{ artifactType, layer string }{{cosignBundle, "signature"}, {"application/vnd.in-toto+json", "attestation"}} {
		b, rd := referrerOf(t, d, r.artifactType, r.layer)
		s.expect(t, r.layer, http.MethodPut, "/manifests/"+rd.String(), b, http.StatusCreated)
		s.refs = append(s.refs, rd)
	}
	legacy := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":` +
		`{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},"layers":[]}`)
	s.expect(t, "cosign's legacy signature", http.MethodPut, "/manifests/"+s.legacyTag(), legacy, http.StatusCreated)
	s.refs = append(s.refs, digest.FromBytes(legacy))
	return s
}

func (s *deleteSetup) legacyTag() string { return "sha256-" + s.subject.Encoded() + ".sig" }

// expect sends method to the path below the version's repository and fails the test unless
// the answer is status; it returns the body.
func (s *deleteSetup) expect(t *testing.T, what, method, path string, body []byte, status int) []byte {
	t.Helper()
	var hdr []string
	if method == http.MethodPut {
		hdr = []string{"Content-Type", ocispecManifest}
	}
	resp, b := request(t, method, s.name+path, bytes.NewReader(body), int64(len(body)), hdr...)
	if resp.StatusCode != status {
		t.Fatalf("%s: %s %s: %d, want %d: %s\n%s", what, method, path, resp.StatusCode, status, b, s.logs)
	}
	return b
}

// referrers returns the referrers the facade lists for the version.
func (s *deleteSetup) referrers(t *testing.T) []string {
	t.Helper()
	return referrersOf(t, "referrers", s.expect(t, "referrers", http.MethodGet, "/referrers/"+s.subject.String(), nil, http.StatusOK))
}

// kept checks that a refused delete changed nothing: the version is in the index and the
// backend, with its two referrers listed and cosign's tag.
func (s *deleteSetup) kept(t *testing.T) {
	t.Helper()
	pkg, err := s.st.GetPackage(t.Context(), 1, s.f.path)
	must(t, err)
	if _, err := s.st.GetVersionByTag(t.Context(), pkg.ID, s.f.tag()); err != nil {
		t.Errorf("the version left the index: %v", err)
	}
	s.expect(t, "the version", http.MethodGet, "/manifests/"+s.f.tag(), nil, http.StatusOK)
	s.expect(t, "cosign's tag", http.MethodGet, "/manifests/"+s.legacyTag(), nil, http.StatusOK)
	if got := s.referrers(t); len(got) != 2 {
		t.Errorf("referrers after a refused delete: %q", got)
	}
}

// TestIntegrationDelete is #61's criterion on a registry that deletes (Zot, distribution with
// deletes on): a signed and attested version deleted through /v2 is 202; its referrers are
// gone, no sha256-<hex> tag is left (the tag-schema index on a registry without the referrers
// API, cosign's legacy tag), the index has no row of it, and the audit row counts three.
func TestIntegrationDelete(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	s := newDeleteSetup(t, addr, config.CapabilityOverrides{})
	if got := s.referrers(t); len(got) != 2 {
		t.Fatalf("referrers before the delete: %q", got)
	}
	s.expect(t, "DELETE", http.MethodDelete, "/manifests/"+s.f.tag(), nil, http.StatusAccepted)
	if got := s.referrers(t); len(got) != 0 {
		t.Errorf("referrers after the delete: %q", got)
	}
	for _, ref := range []string{s.f.tag(), s.subject.String(), "sha256-" + s.subject.Encoded(), s.legacyTag(),
		s.refs[0].String(), s.refs[1].String(), s.refs[2].String()} {
		s.expect(t, "after the delete", http.MethodGet, "/manifests/"+ref, nil, http.StatusNotFound)
	}
	if _, err := s.st.GetPackage(t.Context(), 1, s.f.path); err == nil {
		t.Error("the package is still in the index")
	}
	audit, err := s.st.ListAudit(t.Context(), index.AuditFilter{})
	must(t, err)
	var details map[string]int
	must(t, json.Unmarshal(audit[0].Details, &details))
	if audit[0].Action != "delete" || audit[0].Digest != s.subject || details["referrers"] != 3 {
		t.Errorf("audit event %+v, details %v", audit[0], details)
	}
}

// TestIntegrationDeleteUnsupported: with capabilities.deleteManifest false the delete is 405
// UNSUPPORTED before any request to the registry, and nothing changes.
func TestIntegrationDeleteUnsupported(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	no := false
	s := newDeleteSetup(t, addr, config.CapabilityOverrides{DeleteManifest: &no})
	s.expect(t, "DELETE", http.MethodDelete, "/manifests/"+s.f.tag(), nil, http.StatusMethodNotAllowed)
	s.kept(t)
}

// TestIntegrationDeleteRefused is #61's 405 criterion on distribution with deletes off
// (REGISTRY_STORAGE_DELETE_ENABLED=false) at LAJSTROM_TEST_REGISTRY_NODELETE: the provider
// learns it from the first delete, which is a plain one (cosign's legacy signature), so the
// tag-schema index was not rewritten; the answer is 405 UNSUPPORTED and nothing changed, and
// so is the next delete, which asks the registry nothing.
func TestIntegrationDeleteRefused(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY_NODELETE")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY_NODELETE=host:port to a registry that refuses deletes")
	}
	for _, ref := range []func(*deleteSetup) string{
		func(s *deleteSetup) string { return s.f.tag() },
		func(s *deleteSetup) string { return s.refs[0].String() }, // a signature alone: its subject's index goes first
	} {
		s := newDeleteSetup(t, addr, config.CapabilityOverrides{})
		for _, what := range []string{"first DELETE", "second DELETE"} {
			b := s.expect(t, what, http.MethodDelete, "/manifests/"+ref(s), nil, http.StatusMethodNotAllowed)
			if !bytes.Contains(b, []byte(codeUnsupported)) {
				t.Errorf("%s: %s", what, b)
			}
			s.kept(t)
		}
	}
}

// TestIntegrationDeleteReferrer: a signature deleted on its own leaves the version and its
// other referrers, listed by the referrers API or by the tag-schema index written again.
func TestIntegrationDeleteReferrer(t *testing.T) {
	addr := os.Getenv("LAJSTROM_TEST_REGISTRY")
	if addr == "" {
		t.Skip("set LAJSTROM_TEST_REGISTRY=host:port to run against a registry")
	}
	s := newDeleteSetup(t, addr, config.CapabilityOverrides{})
	s.expect(t, "DELETE of the signature", http.MethodDelete, "/manifests/"+s.refs[0].String(), nil, http.StatusAccepted)
	if got := s.referrers(t); len(got) != 1 || got[0] != s.refs[1].String()+" application/vnd.in-toto+json" {
		t.Errorf("referrers after the signature's delete: %q", got)
	}
	s.expect(t, "the version", http.MethodGet, "/manifests/"+s.f.tag(), nil, http.StatusOK)
}
