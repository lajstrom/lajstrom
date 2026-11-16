package storetest

import (
	"errors"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// blobRow is the row of an encrypted layer of repoID at path, its plaintext of plainOf and its
// ciphertext of storedOf, indexed at indexed seconds.
func blobRow(repoID int64, path, plainOf, storedOf string, indexed int) index.BlobMap {
	return index.BlobMap{RepoID: repoID, Path: path, Kind: index.BlobMapBlob, Plain: dg(plainOf), Stored: dg(storedOf), Size: 1234,
		KeyFingerprint: "8C849E50", Wrapped: "eyJ3cmFwcGVkIjp0cnVlfQ==", PubOpts: "eyJjaXBoZXIiOiJBRVMifQ==", IndexedAt: at(indexed)}
}

// blobMapRows puts the rows of testBlobMap: in the repo raw/secrets two uploads of one file at
// dist, another at docs, a manifest at dist; in raw/other the same ciphertext digest at dist.
func blobMapRows(t *testing.T, s index.Store) (repo, other index.Repo, rows []index.BlobMap) {
	t.Helper()
	repo = newRepo(t, s, "raw", "secrets")
	other = newRepo(t, s, "raw", "other")
	a := blobRow(repo.ID, "dist", "file", "cipher 1", 100)
	again := blobRow(repo.ID, "dist", "file", "cipher 2", 200) // the same bytes uploaded again
	elsewhere := blobRow(repo.ID, "docs", "file", "cipher 3", 100)
	manifest := index.BlobMap{RepoID: repo.ID, Path: "dist", Kind: index.BlobMapManifest, Plain: dg("client manifest"),
		Stored: dg("stored manifest"), Size: 700, IndexedAt: at(100)}
	foreign := blobRow(other.ID, "dist", "file", "cipher 1", 100)
	rows = []index.BlobMap{a, again, elsewhere, manifest, foreign}
	must(t, s.PutBlobMap(t.Context(), rows))
	for _, r := range rows {
		if r.ID == 0 {
			t.Fatalf("PutBlobMap did not fill the id: %+v", r)
		}
	}
	return repo, other, rows
}

// testBlobMap: rows are found by their stored digest at a path, by their plaintext digest there,
// the newest first and of a kind, and listed by path.
func testBlobMap(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo, _, rows := blobMapRows(t, s)
	a := rows[0]

	got, err := s.GetBlobMap(ctx, repo.ID, "dist", dg("cipher 1"))
	must(t, err)
	if got.Plain != dg("file") || got.Size != 1234 || got.Wrapped != a.Wrapped || got.PubOpts != a.PubOpts ||
		got.KeyFingerprint != a.KeyFingerprint || !got.IndexedAt.Equal(at(100)) || got.Kind != index.BlobMapBlob {
		t.Errorf("GetBlobMap = %+v, want %+v", got, a)
	}
	if _, err := s.GetBlobMap(ctx, repo.ID, "docs", dg("cipher 1")); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("GetBlobMap at another path: %v, want ErrNotFound", err)
	}
	all, err := s.ListBlobMap(ctx, repo.ID, "")
	must(t, err)
	if len(all) != 4 || all[0].Path != "dist" || all[3].Path != "docs" {
		t.Errorf("ListBlobMap(every path) = %d rows %+v", len(all), all)
	}
	dist, err := s.ListBlobMap(ctx, repo.ID, "dist")
	must(t, err)
	if len(dist) != 3 {
		t.Errorf("ListBlobMap(dist) = %d rows", len(dist))
	}
}

// testBlobMapFind: the rows of a plaintext digest at a path, the newest first, of a kind.
func testBlobMapFind(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo, _, _ := blobMapRows(t, s)
	found, err := s.FindBlobMap(ctx, repo.ID, "dist", dg("file"), "")
	must(t, err)
	if len(found) != 2 || found[0].Stored != dg("cipher 2") || found[1].Stored != dg("cipher 1") {
		t.Errorf("FindBlobMap(file) = %+v, want cipher 2 then cipher 1", found)
	}
	found, err = s.FindBlobMap(ctx, repo.ID, "dist", dg("client manifest"), index.BlobMapBlob)
	must(t, err)
	if len(found) != 0 {
		t.Errorf("FindBlobMap of a manifest as a blob = %+v", found)
	}
	found, err = s.FindBlobMap(ctx, repo.ID, "dist", dg("client manifest"), index.BlobMapManifest)
	must(t, err)
	if len(found) != 1 || found[0].Stored != dg("stored manifest") {
		t.Errorf("FindBlobMap(client manifest) = %+v", found)
	}
}

// testBlobMapSweep: a put of the same stored digest replaces the row, a sweep keeps what was seen
// and the blobs newer than its bound, a delete removes one row; a repo's rows go with
// ResetDerived and with the repo.
func testBlobMapSweep(t *testing.T, s index.Store) {
	ctx := t.Context()
	repo, other, rows := blobMapRows(t, s)
	// A rotation wraps a layer again: the put of its stored digest replaces the row.
	rewrapped := rows[0]
	rewrapped.ID, rewrapped.KeyFingerprint, rewrapped.Wrapped, rewrapped.IndexedAt = 0, "NEWKEY", "bmV3", at(300)
	must(t, s.PutBlobMap(ctx, []index.BlobMap{rewrapped}))
	got, err := s.GetBlobMap(ctx, repo.ID, "dist", dg("cipher 1"))
	must(t, err)
	if got.KeyFingerprint != "NEWKEY" || got.Wrapped != "bmV3" || got.ID != rows[0].ID || !got.IndexedAt.Equal(at(300)) {
		t.Errorf("after the put again: %+v", got)
	}

	// The sweep of dist at 250 keeps the row seen at 300 and the blob newer than 150, and
	// removes the manifest of 100.
	n, err := s.SweepBlobMap(ctx, repo.ID, "dist", at(250), at(150))
	must(t, err)
	var dist []index.BlobMap
	if n != 1 {
		t.Errorf("SweepBlobMap removed %d rows, want the manifest", n)
	}
	dist, err = s.ListBlobMap(ctx, repo.ID, "dist")
	must(t, err)
	if len(dist) != 2 {
		t.Errorf("after the sweep: %+v", dist)
	}
	must(t, s.DeleteBlobMap(ctx, repo.ID, "dist", dg("cipher 2")))
	must(t, s.DeleteBlobMap(ctx, repo.ID, "dist", dg("cipher 2")))
	if _, err := s.GetBlobMap(ctx, repo.ID, "dist", dg("cipher 2")); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("after the delete: %v", err)
	}
	if _, err := s.GetBlobMap(ctx, other.ID, "dist", dg("cipher 1")); err != nil {
		t.Errorf("the other repo's row: %v", err)
	}

	must(t, s.ResetDerived(ctx, repo.ID))
	if all, err := s.ListBlobMap(ctx, repo.ID, ""); err != nil || len(all) != 0 {
		t.Errorf("after ResetDerived: %+v, %v", all, err)
	}
	must(t, s.DeleteRepo(ctx, other.ID))
	if all, err := s.ListBlobMap(ctx, other.ID, ""); err != nil || len(all) != 0 {
		t.Errorf("after the repo's delete: %+v, %v", all, err)
	}
}
