package facade

import (
	"net/http"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// TestDeleteJournalEntry: an entry of an npm or a pypi package's journal is not deleted through
// /v2, by its tag or by its digest, 409 DENIED; a version of the package is, and so is a _j tag
// of a format without a journal (raw).
func TestDeleteJournalEntry(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, envOptions{ingest: stubIngester{}})
	npm := index.Repo{Format: "npm", Name: "internal", Registry: "local", Prefix: "npm/internal", Kind: index.KindHosted}
	must(t, e.st.CreateRepo(ctx, &npm))
	pkg := index.Package{RepoID: npm.ID, Name: "@acme/widgets", NameOCI: "acme/widgets"}
	must(t, e.st.UpsertPackage(ctx, &pkg))
	entry := digest.FromString("entry 1")
	must(t, e.st.UpsertJournal(ctx, &index.JournalEntry{PackageID: pkg.ID, Seq: 1, Tag: profile.JournalTag(1), Digest: entry,
		ArtifactType: profile.ArtifactTypeNpmDistTags}))
	pypi := index.Repo{Format: "pypi", Name: "internal", Registry: "local", Prefix: "pypi/internal", Kind: index.KindHosted}
	must(t, e.st.CreateRepo(ctx, &pypi))
	requests := index.Package{RepoID: pypi.ID, Name: "requests", NameOCI: "requests"}
	must(t, e.st.UpsertPackage(ctx, &requests))
	yank := digest.FromString("yank 1")
	must(t, e.st.UpsertJournal(ctx, &index.JournalEntry{PackageID: requests.ID, Seq: 1, Tag: profile.JournalTag(1), Digest: yank,
		ArtifactType: profile.ArtifactTypePypiYank}))
	for _, tc := range []struct {
		path, ref string
		status    int
	}{
		{"/v2/npm/internal/acme/widgets", profile.JournalTag(1), http.StatusConflict},
		{"/v2/npm/internal/acme/widgets", profile.JournalTag(7), http.StatusConflict},
		{"/v2/npm/internal/acme/widgets", entry.String(), http.StatusConflict},
		{"/v2/npm/internal/acme/widgets", "1.0.0", http.StatusAccepted},
		{"/v2/npm/internal/acme/widgets", digest.FromString("a version").String(), http.StatusAccepted},
		{"/v2/raw/assets/docs/x.txt", profile.JournalTag(1), http.StatusAccepted},
		{"/v2/pypi/internal/requests", profile.JournalTag(1), http.StatusConflict},
		{"/v2/pypi/internal/requests", yank.String(), http.StatusConflict},
		{"/v2/pypi/internal/requests", "2.32.0__sdist", http.StatusAccepted},
	} {
		rr := e.do(e.h, http.MethodDelete, tc.path+"/manifests/"+tc.ref, nil, principalHeader, "alice")
		if tc.status == http.StatusConflict {
			wantError(t, tc.ref, rr, tc.status, codeDenied)
			continue
		}
		wantStatus(t, tc.path+" "+tc.ref, rr, tc.status)
	}
}
