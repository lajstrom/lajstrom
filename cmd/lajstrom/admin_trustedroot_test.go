package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/tuf"

	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
)

// TestTrustedRootFetch: admin trusted-root fetch writes the trusted root it fetched once it parsed
// it, from the public-good repository by default and from another one with its root; a file it
// does not take, a failed fetch and a missing flag write nothing. The fetch is a stand-in: the
// test makes no network request.
func TestTrustedRootFetch(t *testing.T) {
	tr := attesttest.NewSigstore(t).TrustedRoot(t)
	dir := t.TempDir()
	tufRoot := filepath.Join(dir, "root.json")
	if err := os.WriteFile(tufRoot, []byte(`{"signed":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name    string
		args    []string
		fetched []byte
		failed  error
		url     string // the repository asked; "" when nothing is fetched
		want    string // a part of the error; "" for success
	}{
		{"the public-good instance", []string{"--out", "trusted_root.json"}, tr, nil, tuf.DefaultMirror, ""},
		{"another instance", []string{"--out", "trusted_root.json", "--tuf-url", "https://tuf.example.com", "--tuf-root", tufRoot}, tr, nil,
			"https://tuf.example.com", ""},
		{"another instance without its root", []string{"--out", "trusted_root.json", "--tuf-url", "https://tuf.example.com"}, tr, nil, "", "--tuf-root"},
		{"no --out", nil, tr, nil, "", "--out"},
		{"not a trusted root", []string{"--out", "trusted_root.json"}, []byte(`{"mediaType":"x"}`), nil, tuf.DefaultMirror, "is not a trusted root"},
		{"the fetch fails", []string{"--out", "trusted_root.json"}, nil, errors.New("no route"), tuf.DefaultMirror, "no route"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wd := t.TempDir()
			t.Chdir(wd)
			asked := ""
			cmd := newTrustedRootFetchCmd(func(o *tuf.Options) ([]byte, error) {
				asked = o.RepositoryBaseURL
				if !o.DisableLocalCache {
					t.Error("the TUF client would cache in the home directory")
				}
				return tt.fetched, tt.failed
			})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if asked != tt.url {
				t.Errorf("fetched from %q, want %q", asked, tt.url)
			}
			written, rerr := os.ReadFile(filepath.Join(wd, "trusted_root.json")) //nolint:gosec // the test's own file
			if tt.want == "" {
				if err != nil || rerr != nil || !bytes.Equal(written, tr) || !strings.Contains(out.String(), "1 certificate authorities") {
					t.Errorf("fetch: %v; file %v; output %q", err, rerr, out.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("fetch = %v, want an error with %q", err, tt.want)
			}
			if rerr == nil {
				t.Error("a failed fetch wrote the file")
			}
			if entries, _ := os.ReadDir(wd); len(entries) != 0 {
				t.Errorf("a failed fetch left %d files", len(entries))
			}
		})
	}
}
