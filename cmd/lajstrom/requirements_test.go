package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// TestStartRequirements: serve warns once per repo whose requirements name signers its trust
// does not have, naming them, and of no other repo, and invalidates the documents of the repos
// with requirements, and of no other (docs/spec/policy.md §11).
func TestStartRequirements(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(t.TempDir(), "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := func(name, policy string) index.Repo {
		t.Helper()
		r := index.Repo{Format: "raw", Name: name, Registry: "local", Prefix: "raw/" + name, Kind: index.KindHosted, Policy: json.RawMessage(policy)}
		if err := st.CreateRepo(ctx, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	signed := repo("signed", `{"requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": ["release"]}]}`)
	repo("stale", `{"requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": ["release", "old"]},`+
		` {"predicateType": "https://slsa.dev/provenance", "signedBy": ["old", "gone"]}]}`)
	plain := repo("plain", `{"enforcePull": true}`)
	trust, err := attest.NewTrust([]attest.Entry{attesttest.NewKey(t).Entry(t, "release")})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	startRequirements(ctx, st, trust, slog.New(slog.NewTextHandler(&logs, nil)))
	got := logs.String()
	if strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, "repo=raw/stale") || !strings.Contains(got, "signedBy=\"[old gone]\"") {
		t.Errorf("logged %q, want one warning of raw/stale naming old and gone", got)
	}
	for r, want := range map[*index.Repo]int64{&signed: 1, &plain: 0} {
		if gen, err := st.IndexCacheGeneration(ctx, r.ID); err != nil || gen != want {
			t.Errorf("the generation of the documents of %s: %d, %v; want %d", r.Name, gen, err, want)
		}
	}
}
