package terraform

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// fakeTokens are ArchiveTokens whose token is "<principal>|<ref>".
type fakeTokens struct{}

func (fakeTokens) Issue(_ context.Context, _ *http.Request, p auth.Principal, ref string) (string, error) {
	return p.String() + "|" + ref, nil
}

func (fakeTokens) Allows(_ context.Context, _ *http.Request, raw, ref string) (bool, error) {
	_, got, ok := strings.Cut(raw, "|")
	return ok && got == ref, nil
}

// readers is an Authorizer that lets alice do anything and others nothing, as the bindings of a
// repo without anonymous read would.
type readers struct{}

func (readers) Allowed(_ context.Context, p auth.Principal, _ auth.Action, _, _ string) (bool, error) {
	return p.Name == "alice", nil
}

// newTokenEnv is newEnv with fakeTokens and readers.
func newTokenEnv(t *testing.T) *env {
	t.Helper()
	e := &env{ingestEnv: newIngestEnv(t), t: t}
	deps := format.Deps{Index: e.st, Ingest: e.pipe, Providers: provider.Map{"local": e.prov}, Auth: readers{}, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{Tokens: fakeTokens{}}.Routes(format.NewRouter(mux, Name, deps), deps)
	e.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.Anonymous
		if name := r.Header.Get("X-Test-User"); name != "" {
			p = auth.Principal{Kind: auth.KindUser, Name: name}
		}
		mux.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
	return e
}

// TestSignedArchive: the download of a principal writes a token into X-Terraform-Get, with which
// the archive is served to a request without credentials, as go-getter sends it; a token of
// another version, or none, gets the denial of read.
func TestSignedArchive(t *testing.T) {
	e := newTokenEnv(t)
	one, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	two, _ := testutil.TerraformModule(testutil.TerraformOptions{Files: map[string]string{"main.tf": "# two\n"}})
	for v, b := range map[string][]byte{"1.0.0": one, "1.1.0": two} {
		if rr := e.publish("vpc/aws", v, b); rr.Code != http.StatusCreated {
			t.Fatalf("PUT %s = %d %s", v, rr.Code, rr.Body)
		}
	}
	rr := e.send(http.MethodGet, "vpc/aws/1.0.0/download", nil, "X-Test-User", "alice")
	loc, err := url.Parse(rr.Header().Get("X-Terraform-Get"))
	if err != nil || rr.Code != http.StatusNoContent || loc.Path != "/terraform/infra/vpc/aws/1.0.0/vpc-aws-1.0.0.tar.gz" ||
		loc.Query().Get("token") != "user:alice|terraform/infra/vpc/aws:1.0.0" {
		t.Fatalf("download = %d, X-Terraform-Get %q", rr.Code, rr.Header().Get("X-Terraform-Get"))
	}
	signed := strings.TrimPrefix(loc.RequestURI(), "/terraform/infra/")
	if rr := e.send(http.MethodGet, signed, nil); rr.Code != http.StatusOK || rr.Body.Len() != len(one) {
		t.Errorf("GET the signed archive without credentials = %d, %d bytes", rr.Code, rr.Body.Len())
	}
	if rr := e.send(http.MethodHead, signed, nil); rr.Code != http.StatusOK {
		t.Errorf("HEAD the signed archive = %d", rr.Code)
	}
	if rr := e.send(http.MethodGet, signed, nil, "X-Test-User", "bob"); rr.Code != http.StatusOK {
		t.Errorf("GET the signed archive as bob, who may not read the repo = %d", rr.Code)
	}
	other := "vpc/aws/1.1.0/vpc-aws-1.1.0.tar.gz?" + loc.RawQuery
	for _, c := range []struct {
		path, user string
		status     int
	}{
		{other, "", http.StatusUnauthorized},
		{other, "bob", http.StatusNotFound},
		{"vpc/aws/1.0.0/vpc-aws-1.0.0.tar.gz", "", http.StatusUnauthorized},
		{"vpc/aws/1.0.0/vpc-aws-1.0.0.tar.gz?token=forged", "", http.StatusUnauthorized},
		{"vpc/aws/1.0.0/download", "", http.StatusUnauthorized},
	} {
		hdr := []string{}
		if c.user != "" {
			hdr = []string{"X-Test-User", c.user}
		}
		if rr := e.send(http.MethodGet, c.path, nil, hdr...); rr.Code != c.status {
			t.Errorf("GET %s as %q = %d, want %d", c.path, c.user, rr.Code, c.status)
		}
	}
}

// TestAnonymousDownloadUnsigned: the anonymous principal of a repo with anonymous read gets the
// archive's path alone: it has no credentials to carry over.
func TestAnonymousDownloadUnsigned(t *testing.T) {
	e := newIngestEnv(t)
	one, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	if _, err := e.stream(t.Context(), one, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	deps := format.Deps{Index: e.st, Providers: provider.Map{"local": e.prov}, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{Tokens: fakeTokens{}}.Routes(format.NewRouter(mux, Name, deps), deps)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/terraform/infra/vpc/aws/1.0.0/download", nil)
	mux.ServeHTTP(rr, req.WithContext(auth.WithPrincipal(req.Context(), auth.Anonymous)))
	if loc := rr.Header().Get("X-Terraform-Get"); rr.Code != http.StatusNoContent || loc != "/terraform/infra/vpc/aws/1.0.0/vpc-aws-1.0.0.tar.gz" {
		t.Errorf("anonymous download = %d, X-Terraform-Get %q", rr.Code, loc)
	}
}
