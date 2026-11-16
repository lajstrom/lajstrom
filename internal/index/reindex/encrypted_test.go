package reindex_test

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// encrypted makes the env's repo encrypt at rest, with keys over its store under the master key
// hex, and gives its pipeline and worker those keys; it returns them.
func (e *env) encrypted(t *testing.T, hex string) *keys.Encryption {
	t.Helper()
	m, err := keys.ParseMaster(hex)
	must(t, err)
	enc, err := keys.NewEncryption(keys.EncryptionOptions{Index: e.st, Master: m, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	e.repo.Policy = json.RawMessage(`{"encryption":{"mode":"at-rest"}}`)
	must(t, e.st.UpdateRepo(t.Context(), &e.repo))
	e.withKeys(enc)
	return enc
}

// withKeys rebuilds the env's pipeline and worker with enc, nil for none.
func (e *env) withKeys(enc *keys.Encryption) {
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	log := slog.New(slog.DiscardHandler)
	io := ingest.Options{Providers: e.providers, Index: e.st, Plugins: plugins, Log: log}
	o := reindex.Options{Index: e.st, Providers: e.providers, Repairer: nil, Log: log, Now: e.clock.now}
	if enc != nil {
		io.Encryption, o.Keys = enc, enc
	}
	e.pipe = ingest.New(io)
	e.inspect = &counting{Inspector: e.pipe}
	o.Inspector, o.Repairer = e.inspect, e.pipe
	e.w = reindex.New(o)
}

// blobMap is the rows of blob_map of the env's repo, by stored digest, without ids and times.
func (e *env) blobMap(t *testing.T) map[string]index.BlobMap {
	t.Helper()
	rows, err := e.st.ListBlobMap(t.Context(), e.repo.ID, "")
	must(t, err)
	out := map[string]index.BlobMap{}
	for _, r := range rows {
		out[string(r.Stored)] = index.BlobMap{RepoID: r.RepoID, Path: r.Path, Kind: r.Kind, Plain: r.Plain, Stored: r.Stored, Size: r.Size,
			KeyFingerprint: r.KeyFingerprint, Wrapped: r.Wrapped, PubOpts: r.PubOpts}
	}
	return out
}

// TestRebuildEncrypted: an encrypted repo's rows, blob_map among them, dropped and rebuilt by a
// run with the keys, equal to what ingest wrote; the files name the ciphertexts.
func TestRebuildEncrypted(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.encrypted(t, strings.Repeat("0123456789abcdef", 4))
	e.publish(t, "docs/readme.md", "hello, encrypted")
	e.publish(t, "bin/tool", "#!/bin/sh encrypted")
	before, mapBefore := e.derived(t), e.blobMap(t)
	if len(mapBefore) != 6 { // two manifests, two files, two metadata layers
		t.Fatalf("blob_map after the publishes: %d rows", len(mapBefore))
	}
	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	r := e.run(t, reindex.Request{Prune: true})
	if r.Added != 2 || len(r.Errors) != 0 || r.Skipped != "" || r.BlobMap != 6 {
		t.Errorf("report: %+v", *r)
	}
	same(t, "the rebuilt rows", before, e.derived(t))
	same(t, "the rebuilt blob_map", mapBefore, e.blobMap(t))
}

// TestEncryptedWithoutKeys: a run without the master key, or whose master key does not open the
// repo's keys, skips the encrypted repo whole and says why; its rows stay.
func TestEncryptedWithoutKeys(t *testing.T) {
	e := newEnv(t)
	e.encrypted(t, strings.Repeat("0123456789abcdef", 4))
	e.publish(t, "docs/readme.md", "hello, encrypted")
	before, mapBefore := e.derived(t), e.blobMap(t)

	e.withKeys(nil)
	r := e.run(t, reindex.Request{Prune: true})
	if !strings.Contains(r.Skipped, "no master key") || r.Manifests != 0 || r.Removed != 0 {
		t.Errorf("without the master key: %+v", *r)
	}
	other, err := keys.ParseMaster(strings.Repeat("fedcba9876543210", 4))
	must(t, err)
	enc, err := keys.NewEncryption(keys.EncryptionOptions{Index: e.st, Master: other, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	e.withKeys(enc)
	r = e.run(t, reindex.Request{Prune: true})
	if !strings.Contains(r.Skipped, "do not open") || r.Manifests != 0 {
		t.Errorf("with another master key: %+v", *r)
	}
	same(t, "the rows after the skipped runs", before, e.derived(t))
	same(t, "blob_map after the skipped runs", mapBefore, e.blobMap(t))
	if n := e.inspect.calls.Load(); n != 0 {
		t.Errorf("a skipped repo was inspected %d times", n)
	}
}
