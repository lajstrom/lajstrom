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

// TestRotateEncrypted: a rotation retires the key and wraps every data key again for the new one
// in a keys referrer per manifest, without pushing a blob or a version's manifest; run again it
// wraps nothing; the files still open, and a rebuild takes the newest wrapping.
func TestRotateEncrypted(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	enc := e.encrypted(t, strings.Repeat("0123456789abcdef", 4))
	e.publish(t, "docs/readme.md", "hello, rotated")
	e.publish(t, "bin/tool", "#!/bin/sh rotated")
	pushesBefore := len(e.fake.Pushes())
	old, err := e.st.GetKey(ctx, &e.repo.ID, keys.KindEnc)
	must(t, err)

	row, retired, err := enc.Rotate(ctx, e.repo)
	must(t, err)
	if retired != old.Fingerprint || row.Fingerprint == old.Fingerprint {
		t.Fatalf("Rotate = %s retiring %q, want a new key retiring %s", row.Fingerprint, retired, old.Fingerprint)
	}
	n, err := e.pipe.Rewrap(ctx, e.repo, alice)
	must(t, err)
	if n != 2 {
		t.Errorf("Rewrap pushed %d keys referrers, want 2", n)
	}
	before, err := e.st.ListBlobMap(ctx, e.repo.ID, "")
	must(t, err)
	for _, p := range e.fake.Pushes()[pushesBefore:] {
		for _, r := range before {
			if p.Digest == r.Stored {
				t.Errorf("the rotation pushed a blob or a version's manifest again: %+v", p)
			}
		}
	}
	e.checkWrapped(t, enc, row.Fingerprint, "after the rotation")
	if n, err := e.pipe.Rewrap(ctx, e.repo, alice); err != nil || n != 0 {
		t.Errorf("Rewrap again = %d, %v; want nothing to do", n, err)
	}
	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	r := e.run(t, reindex.Request{Prune: true})
	if r.Added != 2 || len(r.Errors) != 0 || r.Attachments != 0 {
		t.Errorf("report after the rotation: %+v", *r)
	}
	e.checkWrapped(t, enc, row.Fingerprint, "after a rebuild")
}

// checkWrapped checks that every row of a blob of the env's repo is wrapped for the key active and
// opens to its plaintext digest.
func (e *env) checkWrapped(t *testing.T, enc *keys.Encryption, active, what string) {
	t.Helper()
	ctx := t.Context()
	rows, err := e.st.ListBlobMap(ctx, e.repo.ID, "")
	must(t, err)
	blobs := 0
	for _, r := range rows {
		if r.Kind != index.BlobMapBlob {
			continue
		}
		blobs++
		if r.KeyFingerprint != active {
			t.Errorf("%s: %s is wrapped for %s, want the new key", what, r.Stored, r.KeyFingerprint)
		}
		opts, err := enc.Unwrap(ctx, e.repo, r.KeyFingerprint, r.Wrapped)
		if err != nil || opts.Digest != r.Plain {
			t.Errorf("%s: %s opens to %v, %v", what, r.Stored, opts, err)
		}
	}
	if blobs != 4 {
		t.Errorf("%s: %d rows of blobs, want 4", what, blobs)
	}
}
