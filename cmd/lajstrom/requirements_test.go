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

// TestWarnRequirements: serve warns once per repo whose requirements name signers its trust
// does not have, naming them, and of no other repo (docs/spec/policy.md §11).
func TestWarnRequirements(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(t.TempDir(), "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := func(name, policy string) {
		t.Helper()
		r := index.Repo{Format: "raw", Name: name, Registry: "local", Prefix: "raw/" + name, Kind: index.KindHosted, Policy: json.RawMessage(policy)}
		if err := st.CreateRepo(ctx, &r); err != nil {
			t.Fatal(err)
		}
	}
	repo("signed", `{"requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": ["release"]}]}`)
	repo("stale", `{"requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": ["release", "old"]},`+
		` {"predicateType": "https://slsa.dev/provenance", "signedBy": ["old", "gone"]}]}`)
	repo("plain", `{"enforcePull": true}`)
	trust, err := attest.NewTrust([]attest.Entry{attesttest.NewKey(t).Entry(t, "release")})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	warnRequirements(ctx, st, trust, slog.New(slog.NewTextHandler(&logs, nil)))
	got := logs.String()
	if strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, "repo=raw/stale") || !strings.Contains(got, "signedBy=\"[old gone]\"") {
		t.Errorf("logged %q, want one warning of raw/stale naming old and gone", got)
	}
}
