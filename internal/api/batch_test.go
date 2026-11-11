package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// scriptedDeleter records the deletions it gets and answers each with the error of its digest.
type scriptedDeleter struct {
	got  []format.Deletion
	errs map[string]error
}

func (d *scriptedDeleter) Delete(_ context.Context, _ index.Repo, del format.Deletion, _ auth.Principal) (*format.Deleted, error) {
	d.got = append(d.got, del)
	return nil, d.errs[del.Reference]
}

// TestBatchDelete: each item is checked as the conditional DELETE of one version is, with its
// digests and the cut-off, never forced, and gets a status of its own: deleted, changed (with
// the digests now), young (with lastModified), protected (with the pointers), gone, error. The
// refusals of ingest under the lock are statuses too; an error of one item does not stop the
// others. A dry run checks and asks nothing of the Deleter. The job goes to every deletion.
func TestBatchDelete(t *testing.T) {
	d := &scriptedDeleter{errs: map[string]error{}}
	f := newFixture(t, Options{Deleter: d})
	repo := f.create(t, "pypi", "libs")
	old := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	dg := func(s string) string { return digest.FromString(s).String() }
	publish(t, f.st, repo.ID, "requests", "1.0", old, "sdist", "py3-none-any")
	publish(t, f.st, repo.ID, "requests", "1.1", old)
	publish(t, f.st, repo.ID, "requests", "1.2", old.Add(60*24*time.Hour))
	pkg := publish(t, f.st, repo.ID, "requests", "1.3", old)
	publish(t, f.st, repo.ID, "requests", "1.4", old)
	publish(t, f.st, repo.ID, "requests", "1.5", old)
	must(t, f.st.ReplaceJournalState(context.Background(), pkg.ID, []index.JournalState{{Kind: index.JournalDistTag, Key: "latest", Value: "1.3", Seq: 1}}))
	d.errs[dg("requests1.4")] = fmt.Errorf("ingest: %w", &format.RefusedError{Reason: format.RefusedChanged, Subject: "version 1.4",
		Digests: []digest.Digest{digest.FromString("again")}})
	d.errs[dg("requests1.5")] = fmt.Errorf("ingest: the registry: %w", provider.ErrUnsupported)
	body := `{"cutoff": "2026-09-01T00:00:00Z", "job": "retention-7", "items": [
		{"package": "requests", "version": "1.0", "digests": ["` + dg("requests1.0py3-none-any") + `", "` + dg("requests1.0sdist") + `"]},
		{"package": "requests", "version": "1.1", "digests": ["` + dg("requests1.1x") + `"]},
		{"package": "requests", "version": "1.2", "digests": ["` + dg("requests1.2") + `"]},
		{"package": "requests", "version": "1.3", "digests": ["` + dg("requests1.3") + `"]},
		{"package": "requests", "version": "9.9", "digests": ["` + dg("requests9.9") + `"]},
		{"package": "flask", "version": "1.0", "digests": ["` + dg("flask1.0") + `"]},
		{"package": "requests", "version": "1.4", "digests": ["` + dg("requests1.4") + `"]},
		{"package": "requests", "version": "1.5", "digests": ["` + dg("requests1.5") + `"]}]`
	var dry BatchDeleteResult
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/repos/pypi/libs/versions:delete", body+`, "dryRun": true}`, admin), http.StatusOK, &dry)
	if len(d.got) != 0 {
		t.Errorf("a dry run deleted %+v", d.got)
	}
	if got := statuses(dry); got != "deleted changed young protected gone gone deleted deleted" || !dry.DryRun || dry.Job != "retention-7" {
		t.Errorf("dry run: %s, %+v", got, dry)
	}

	var res BatchDeleteResult
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/repos/pypi/libs/versions:delete", body+`}`, admin), http.StatusOK, &res)
	if got := statuses(res); got != "deleted changed young protected gone gone changed error" || res.DryRun {
		t.Fatalf("statuses %s", got)
	}
	r := res.Results
	if len(r[1].Digests) != 1 || r[1].Digests[0] != dg("requests1.1") || !strings.Contains(r[1].Message, "changed since it was read") {
		t.Errorf("changed: %+v", r[1])
	}
	if r[2].LastModified == nil || !r[2].LastModified.Equal(old.Add(60*24*time.Hour)) || !strings.Contains(r[2].Message, "after the cut-off") {
		t.Errorf("young: %+v", r[2])
	}
	if len(r[3].Pointers) != 1 || r[3].Pointers[0] != (format.Pointer{Kind: "dist-tag", Name: "latest"}) {
		t.Errorf("protected: %+v", r[3])
	}
	if r[6].Digests[0] != digest.FromString("again").String() || !strings.Contains(r[7].Message, "does not delete manifests") || r[0].Message != "" {
		t.Errorf("refused under the lock, failed: %+v, %+v; deleted %+v", r[6], r[7], r[0])
	}
	var refs []string
	for _, del := range d.got {
		if del.Job != "retention-7" || del.Condition == nil || del.Condition.Force || !del.Condition.UnmodifiedSince.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("deletion %+v, condition %+v", del, del.Condition)
		}
		refs = append(refs, del.Reference)
	}
	want := []string{dg("requests1.0py3-none-any"), dg("requests1.0sdist"), dg("requests1.4"), dg("requests1.5")}
	slices.Sort(refs)
	slices.Sort(want)
	if !slices.Equal(refs, want) {
		t.Errorf("deleted %v, want %v", refs, want)
	}
}

// statuses are the statuses of res's results, joined by spaces.
func statuses(res BatchDeleteResult) string {
	var s []string
	for _, r := range res.Results {
		s = append(s, r.Status)
	}
	return strings.Join(s, " ")
}

// TestBatchDeleteRequest: the body's rules, each 400 naming the field, before anything is
// deleted; a job of the server's when the client names none; delete on the repo before the
// body is read; 404 for a repo that does not exist.
func TestBatchDeleteRequest(t *testing.T) {
	d := &scriptedDeleter{}
	f := newFixture(t, Options{Deleter: d, Auth: fakeAuth{grants: map[string]bool{
		"user:admin delete pypi/libs": true, "user:admin read pypi/libs": true, "user:alice read pypi/libs": true,
		"user:admin delete */*": true}}})
	repo := f.create(t, "pypi", "libs")
	publish(t, f.st, repo.ID, "requests", "1.0", testNow)
	target := "/api/v1/repos/pypi/libs/versions:delete"
	item := `{"package": "requests", "version": "1.0", "digests": ["` + digest.FromString("requests1.0").String() + `"]}`
	for _, tt := range []struct{ body, want string }{
		{`{"items": []}`, "items: at least one"},
		{`{"items": [{"version": "1.0", "digests": ["sha256:00"]}]}`, "items[0].package"},
		{`{"items": [{"package": "requests", "digests": ["sha256:00"]}]}`, "items[0].version"},
		{`{"items": [{"package": "requests", "version": "1.0", "digests": []}]}`, "items[0].digests"},
		{`{"items": [` + item + `, {"package": "requests", "version": "1.0", "digests": ["1.0"]}]}`, "items[1].digests[0]"},
		{`{"items": [` + item + `], "job": "-x"}`, "job:"},
		{`{"items": [` + item + `], "cutoff": "yesterday"}`, "cutoff"},
		{`{"items": [` + item + `], "force": true}`, "force"},
	} {
		wantError(t, f.do(http.MethodPost, target, tt.body, admin), http.StatusBadRequest, codeInvalid, tt.want)
	}
	if len(d.got) != 0 {
		t.Fatalf("deletions %+v", d.got)
	}
	wantError(t, f.do(http.MethodPost, target, "not json", alice), http.StatusForbidden, codeForbidden)
	wantError(t, f.do(http.MethodPost, target, "not json", bob), http.StatusNotFound, codeNotFound)
	wantError(t, f.do(http.MethodPost, "/api/v1/repos/pypi/none/versions:delete", `{"items": [`+item+`]}`, admin), http.StatusNotFound, codeNotFound,
		"repo pypi/none does not exist")
	var res BatchDeleteResult
	decodeJSON(t, f.do(http.MethodPost, target, `{"items": [`+item+`]}`, admin), http.StatusOK, &res)
	if res.Job == "" || len(d.got) != 1 || d.got[0].Job != res.Job || res.Results[0].Status != statusDeleted {
		t.Errorf("job %q, deletions %+v, results %+v", res.Job, d.got, res.Results)
	}
	wantError(t, newFixture(t, Options{}).do(http.MethodPost, target, `{"items": [`+item+`]}`, admin), http.StatusNotImplemented, codeUnavailable, "not enabled")
}
