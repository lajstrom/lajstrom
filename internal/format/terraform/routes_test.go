package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// env is the terraform routes over the real pipeline.
type env struct {
	*ingestEnv
	t *testing.T
	h http.Handler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{ingestEnv: newIngestEnv(t), t: t}
	deps := format.Deps{Index: e.st, Ingest: e.pipe, Providers: provider.Map{"local": e.prov}, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	e.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name := r.Header.Get("X-Test-User"); name != "" {
			r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Kind: auth.KindUser, Name: name}))
		}
		if r.Header.Get("X-Test-Repo-Host") != "" {
			r = r.WithContext(server.WithRepoHost(r.Context(), server.RepoHost{Format: Name, Repo: "infra"}))
		}
		mux.ServeHTTP(w, r)
	})
	return e
}

// send sends a request with body, which may be nil, to /terraform/infra/<path>; hdr are header
// name and value pairs.
func (e *env) send(method, path string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/terraform/infra/"+path, body)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// publish publishes b as version of the module path (vpc/aws) as alice.
func (e *env) publish(path, version string, b []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.send(http.MethodPut, path+"/"+version, bytes.NewReader(b), "X-Test-User", "alice")
}

// TestPublishAndInstall publishes two versions and reads them back as Terraform does: the
// versions document, the download's X-Terraform-Get, and the archive at that path; then with the
// namespace that a repo host's discovery adds.
func TestPublishAndInstall(t *testing.T) {
	e := newEnv(t)
	one, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	two, sums := testutil.TerraformModule(testutil.TerraformOptions{Files: map[string]string{"main.tf": "# 1.10.0\n"}})
	rr := e.publish("vpc/aws", "1.10.0", two)
	var got publishResponse
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &got) != nil {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	want := publishResponse{Namespace: "infra", Name: "vpc", System: "aws", Version: "1.10.0", Filename: "vpc-aws-1.10.0.tar.gz",
		Digest: rr.Header().Get("Docker-Content-Digest"), Tag: "1.10.0"}
	if got != want || rr.Header().Get("Location") != "/terraform/infra/vpc/aws/1.10.0/vpc-aws-1.10.0.tar.gz" {
		t.Errorf("PUT answered %+v, Location %q", got, rr.Header().Get("Location"))
	}
	for _, v := range []string{"1.9.0", "1.10.0-rc.1"} {
		if rr := e.publish("vpc/aws", v, one); rr.Code != http.StatusCreated {
			t.Fatalf("PUT %s = %d %s", v, rr.Code, rr.Body)
		}
	}
	for _, prefix := range []string{"", "infra/"} {
		e.install(prefix, two, sums.SHA256, got.Digest)
	}
	if rr := e.send(http.MethodGet, "VPC/aws/versions", nil, "X-Test-User", "alice"); rr.Code != http.StatusOK {
		t.Errorf("GET VPC/aws/versions = %d, want the module vpc/aws", rr.Code)
	}
}

// install reads vpc/aws 1.10.0 as Terraform does, its routes below prefix: the versions document
// of the three versions TestPublishAndInstall published, the download's X-Terraform-Get, and the
// archive at that path, whose bytes are b with the sha256 sum, in the manifest manifest.
func (e *env) install(prefix string, b []byte, sum, manifest string) {
	e.t.Helper()
	rr := e.send(http.MethodGet, prefix+"vpc/aws/versions", nil, "X-Test-User", "alice")
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != jsonContentType ||
		rr.Body.String() != `{"modules":[{"versions":[{"version":"1.9.0"},{"version":"1.10.0-rc.1"},{"version":"1.10.0"}]}]}` {
		e.t.Errorf("GET %sversions = %d %q %s", prefix, rr.Code, rr.Header().Get("Content-Type"), rr.Body)
	}
	rr = e.send(http.MethodGet, prefix+"vpc/aws/1.10.0/download", nil, "X-Test-User", "alice")
	loc := rr.Header().Get("X-Terraform-Get")
	if rr.Code != http.StatusNoContent || loc != "/terraform/infra/vpc/aws/1.10.0/vpc-aws-1.10.0.tar.gz" {
		e.t.Fatalf("GET %sdownload = %d, X-Terraform-Get %q", prefix, rr.Code, loc)
	}
	rr = e.send(http.MethodGet, strings.TrimPrefix(loc, "/terraform/infra/"), nil, "X-Test-User", "alice")
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), b) || rr.Header().Get("Content-Type") != archiveContentType ||
		rr.Header().Get("ETag") != `"sha256:`+sum+`"` || rr.Header().Get("Docker-Content-Digest") != manifest {
		e.t.Errorf("GET the archive = %d, %d bytes, headers %v", rr.Code, rr.Body.Len(), rr.Header())
	}
}

// TestPublishRefuses: a version, a name or a system out of their grammars and a body that is no
// module archive are 400, other bytes under a version and another spelling of a module 409; the
// same bytes again are the repeat.
func TestPublishRefuses(t *testing.T) {
	e := newEnv(t)
	one, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	two, _ := testutil.TerraformModule(testutil.TerraformOptions{Files: map[string]string{"main.tf": "# two\n"}})
	first := e.publish("vpc/aws", "1.0.0", one)
	if first.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", first.Code, first.Body)
	}
	if rr := e.publish("vpc/aws", "1.0.0", one); rr.Code != http.StatusCreated ||
		rr.Header().Get("Docker-Content-Digest") != first.Header().Get("Docker-Content-Digest") {
		t.Errorf("the repeat = %d %s", rr.Code, rr.Header().Get("Docker-Content-Digest"))
	}
	for _, c := range []struct {
		path, version string
		body          []byte
		status        int
		says          string
	}{
		{"vpc/aws", "v1.1.0", one, http.StatusBadRequest, "not a version"},
		{"vpc/aws", "1.1.0+7", one, http.StatusBadRequest, "not a version"},
		{"v.pc/aws", "1.1.0", one, http.StatusBadRequest, "not a module name"},
		{"vpc/AWS", "1.1.0", one, http.StatusBadRequest, "not a target system"},
		{"vpc/aws", "1.1.0", []byte("not a module"), http.StatusBadRequest, "not gzip-compressed"},
		{"vpc/aws", "1.0.0", two, http.StatusConflict, ""},
		{"VPC/aws", "1.1.0", one, http.StatusConflict, "exists as vpc/aws"},
	} {
		rr := e.publish(c.path, c.version, c.body)
		if rr.Code != c.status || !strings.Contains(rr.Body.String(), c.says) {
			t.Errorf("PUT %s/%s = %d %s, want %d saying %q", c.path, c.version, rr.Code, rr.Body, c.status, c.says)
		}
	}
}

// TestNotFound: a module, a version, a file and a namespace the repo does not have are 404.
func TestNotFound(t *testing.T) {
	e := newEnv(t)
	one, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	if rr := e.publish("vpc/aws", "1.0.0", one); rr.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	for _, path := range []string{
		"vpc/gcp/versions", "v.pc/aws/versions", "vpc/aws/2.0.0/download", "vpc/aws/v1.0.0/download",
		"vpc/aws/1.0.0/vpc-aws-1.0.1.tar.gz", "vpc/aws/1.0.0/other.tar.gz", "vpc/aws/2.0.0/vpc-aws-2.0.0.tar.gz",
		"other/vpc/aws/versions", "other/vpc/aws/1.0.0/download",
	} {
		if rr := e.send(http.MethodGet, path, nil, "X-Test-User", "alice"); rr.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d %s, want 404", path, rr.Code, rr.Body)
		}
	}
}

// TestDiscovery: the main host's document and a repo host's, which points at the root of the
// repo host, and the download's X-Terraform-Get there, without /terraform/<repo>.
func TestDiscovery(t *testing.T) {
	rr := httptest.NewRecorder()
	Plugin{}.WellKnown()["terraform.json"].ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/.well-known/terraform.json", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != `{"modules.v1":"/terraform/"}` || rr.Header().Get("Content-Type") != jsonContentType {
		t.Errorf("the main host's discovery = %d %q %s", rr.Code, rr.Header().Get("Content-Type"), rr.Body)
	}
	e := newEnv(t)
	if rr := e.send(http.MethodGet, ".well-known/terraform.json", nil, "X-Test-User", "alice"); rr.Body.String() != `{"modules.v1":"/terraform/infra/"}` {
		t.Errorf("the repo's discovery on the main host = %d %s", rr.Code, rr.Body)
	}
	if rr := e.send(http.MethodGet, ".well-known/terraform.json", nil, "X-Test-User", "alice", "X-Test-Repo-Host", "1"); rr.Body.String() != `{"modules.v1":"/"}` {
		t.Errorf("the repo host's discovery = %d %s", rr.Code, rr.Body)
	}
	one, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	if rr := e.publish("vpc/aws", "1.0.0", one); rr.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	rr = e.send(http.MethodGet, "infra/vpc/aws/1.0.0/download", nil, "X-Test-User", "alice", "X-Test-Repo-Host", "1")
	if loc := rr.Header().Get("X-Terraform-Get"); rr.Code != http.StatusNoContent || loc != "/vpc/aws/1.0.0/vpc-aws-1.0.0.tar.gz" {
		t.Errorf("the download on a repo host = %d, X-Terraform-Get %q", rr.Code, loc)
	}
}

// TestDeleteRendersAgain: a version deleted is gone from the next versions document, and the
// module with its last version.
func TestDeleteRendersAgain(t *testing.T) {
	e := newEnv(t)
	one, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if rr := e.publish("vpc/aws", v, one); rr.Code != http.StatusCreated {
			t.Fatalf("PUT %s = %d %s", v, rr.Code, rr.Body)
		}
	}
	if rr := e.send(http.MethodGet, "vpc/aws/versions", nil, "X-Test-User", "alice"); !strings.Contains(rr.Body.String(), "1.1.0") {
		t.Fatalf("versions = %s", rr.Body)
	}
	del := func(tag string) {
		t.Helper()
		if _, err := e.pipe.Delete(context.Background(), e.repo, format.Deletion{Path: "vpc/aws", Reference: tag}, alice); err != nil {
			t.Fatal(err)
		}
	}
	del("1.1.0")
	if rr := e.send(http.MethodGet, "vpc/aws/versions", nil, "X-Test-User", "alice"); rr.Body.String() != `{"modules":[{"versions":[{"version":"1.0.0"}]}]}` {
		t.Errorf("versions after the delete = %d %s", rr.Code, rr.Body)
	}
	del("1.0.0")
	if rr := e.send(http.MethodGet, "vpc/aws/versions", nil, "X-Test-User", "alice"); rr.Code != http.StatusNotFound {
		t.Errorf("versions after the last delete = %d %s", rr.Code, rr.Body)
	}
}

func TestCompareVersions(t *testing.T) {
	// SemVer 2.0.0 §11's example, shuffled, and numbers longer than an int64.
	sorted := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11",
		"1.0.0-rc.1", "1.0.0", "1.9.0", "1.10.0", "2.0.0", "99999999999999999999.0.0"}
	got := slices.Clone(sorted)
	slices.Reverse(got)
	got[0], got[5] = got[5], got[0]
	slices.SortFunc(got, compareVersions)
	if !slices.Equal(got, sorted) {
		t.Errorf("sorted %v\nwant %v", got, sorted)
	}
}
