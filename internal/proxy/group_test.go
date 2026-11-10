package proxy_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// groupEnv is the env with the hosted repo raw/internal, which holds acme-billing 1.0.0, and
// the group raw/all of internal and the proxy raw/up, in that order. The upstream has tool
// 1.0.0 and 2.0.0, and acme-billing 9.9.9: a public package under the private name.
func groupEnv(t *testing.T, settings string) (*env, index.Repo, index.Repo) {
	t.Helper()
	ctx := t.Context()
	e := newEnv(t, settings)
	e.up.publish("tool", "1.0.0", "one", nil)
	e.up.publish("tool", "2.0.0", "two", nil)
	e.up.publish("acme-billing", "9.9.9", "evil twin", nil)
	internal := index.Repo{Format: "raw", Name: "internal", Registry: "local", Prefix: "raw/internal", Kind: index.KindHosted}
	must(t, e.st.CreateRepo(ctx, &internal))
	up := upload("acme-billing", "1.0.0", "private")
	_, err := e.pipe.Upload(asAlice(t), internal, up, alice)
	must(t, err)
	all := index.Repo{Format: "raw", Name: "all", Prefix: "raw/all", Kind: index.KindGroup, Group: json.RawMessage(`{"merge": []}`)}
	must(t, e.st.CreateRepo(ctx, &all))
	must(t, e.st.SetGroupMembers(ctx, all.ID, []int64{internal.ID, e.repo.ID}))
	all, internal = reread(t, e, all), reread(t, e, internal)
	return e, internal, all
}

// upload is a client's upload of version of name with one file of content.
func upload(name, version, content string) format.Upload {
	return format.Upload{Params: map[string]string{"name": name, "version": version}, Files: []format.UploadedFile{{
		Name: name + "-" + version + ".tgz", Size: int64(len(content)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil }}}}
}

// reread returns the repo as the index has it now.
func reread(t *testing.T, e *env, r index.Repo) index.Repo {
	t.Helper()
	got, err := e.st.GetRepoByID(t.Context(), r.ID)
	must(t, err)
	return *got
}

// versions are the versions a merged listing has, sorted, each with the member it came from.
func versions(t *testing.T, c *index.Cache) []string {
	t.Helper()
	var out []string
	for v, entry := range listing(t, c) {
		fields := strings.Fields(entry)
		out = append(out, v+" "+fields[len(fields)-1])
	}
	slices.Sort(out)
	return out
}

// TestGroupDocument: a group merges its members' documents in order, its links its own; a name
// that the hosted member holds hides the upstream's versions of it, and no request goes there
// for it, unless a merge pattern matches it; the ETag follows the members and the group
// (docs/spec/proxy-and-group.md §8).
func TestGroupDocument(t *testing.T) {
	ctx := t.Context()
	e, _, all := groupEnv(t, `{"name": "up"}`)
	c := e.core(t, "r1", proxy.Options{})

	doc, err := c.Document(ctx, all, "tool", docKind)
	must(t, err)
	if got := versions(t, doc); !slices.Equal(got, []string{"1.0.0 up", "2.0.0 up"}) {
		t.Errorf("tool through the group: %v", got)
	}
	if l := listing(t, doc); !strings.HasPrefix(l["1.0.0"], "https://art.test/raw/all/tool/-/tool-1.0.0.tgz ") {
		t.Errorf("a link of the group: %q, want it under the group", l["1.0.0"])
	}
	if doc.RepoID != all.ID || doc.ID != 0 {
		t.Errorf("the group's document %+v, want the group's and stored nowhere", doc)
	}
	if _, err := e.st.GetIndexCache(ctx, all.ID, "tool", docKind); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the group stored its document: %v", err)
	}

	calls := e.up.metaCalls.Load()
	doc, err = c.Document(ctx, all, "acme-billing", docKind)
	must(t, err)
	if got := versions(t, doc); !slices.Equal(got, []string{"1.0.0 internal"}) {
		t.Errorf("a name the hosted member holds: %v, want its versions alone", got)
	}
	if n := e.up.metaCalls.Load() - calls; n != 0 {
		t.Errorf("%d upstream requests for a name the hosted member holds", n)
	}
	again, err := c.Document(ctx, all, "acme-billing", docKind)
	must(t, err)
	if again.SHA256 != doc.SHA256 {
		t.Errorf("the ETag moved without a change: %s, %s", doc.SHA256, again.SHA256)
	}

	must(t, e.st.UpdateRepo(ctx, func() *index.Repo { all.Group = json.RawMessage(`{"merge": ["ACME-*"]}`); return &all }()))
	merged, err := c.Document(ctx, all, "acme-billing", docKind)
	must(t, err)
	if got := versions(t, merged); !slices.Equal(got, []string{"1.0.0 internal", "9.9.9 up"}) {
		t.Errorf("a name of group.merge: %v, want both members' versions", got)
	}
	if merged.SHA256 == doc.SHA256 {
		t.Error("the ETag did not move with the group")
	}

	if _, err := c.Document(ctx, all, "nope", docKind); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a name no member has: %v, want ErrNotFound", err)
	}
}

// TestGroupDocumentFailures: a proxy member that fails leaves the answer, which comes from the
// members that have the name; without one, its failure is the answer; a member whose lists
// refuse the name does not have it (§8.1).
func TestGroupDocumentFailures(t *testing.T) {
	ctx := t.Context()
	e, _, all := groupEnv(t, `{"name": "up", "deny": ["tool"]}`)
	c := e.core(t, "r1", proxy.Options{})
	if _, err := c.Document(ctx, all, "tool", docKind); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a name the proxy member's lists refuse: %v, want ErrNotFound", err)
	}
	e.up.status.Store(http.StatusInternalServerError)
	doc, err := c.Document(ctx, all, "acme-billing", docKind)
	if err != nil || len(listing(t, doc)) != 1 {
		t.Errorf("the hosted member's name with the upstream down: %v", err)
	}
	all.Group = json.RawMessage(`{"merge": ["acme-*"]}`)
	doc, err = c.Document(ctx, all, "acme-billing", docKind)
	if err != nil || !slices.Equal(versions(t, doc), []string{"1.0.0 internal"}) {
		t.Errorf("a merged name with the proxy member down: %v, want the hosted member's", err)
	}
	_, err = c.Document(ctx, all, "other", docKind)
	if status, _ := statusOf(err); status != http.StatusBadGateway {
		t.Errorf("a name only the failing member could have: %v (%d), want 502", err, status)
	}
}

// TestGroupFile: a file comes from the member that holds it, or is fetched by the proxy member
// through the group, its audit row naming the group; a version that the hosted member's name
// does not have is 404, and the upstream is not asked for it (§8.3).
func TestGroupFile(t *testing.T) {
	e, internal, all := groupEnv(t, `{"name": "up"}`)
	c := e.core(t, "r1", proxy.Options{})

	m, err := c.File(asAlice(t), all, fileOf("acme-billing", "1.0.0"))
	must(t, err)
	if m.Package.RepoID != internal.ID || m.Version.Version != "1.0.0" {
		t.Errorf("the hosted member's file: %+v", m)
	}
	if _, err := c.File(asAlice(t), all, fileOf("acme-billing", "9.9.9")); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the upstream's version of a hosted name: %v, want ErrNotFound", err)
	}
	if n := e.up.fileCalls.Load(); n != 0 {
		t.Errorf("%d file requests to the upstream for a hosted name", n)
	}

	m, err = c.File(asAlice(t), all, fileOf("tool", "2.0.0"))
	must(t, err)
	if m.Package.RepoID != e.repo.ID || m.Version.Version != "2.0.0" {
		t.Errorf("the proxy member's file: %+v", m)
	}
	_, details := audits(t, e, "proxy.fetch")
	if len(details) != 1 || details[0]["via"] != "raw/all" {
		t.Errorf("proxy.fetch through the group: %v, want via raw/all", details)
	}
	if _, err := c.File(asAlice(t), all, fileOf("tool", "3.0.0")); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a version no member has: %v, want ErrNotFound", err)
	}
	if _, err := c.File(asAlice(t), all, "not/a/file"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a path that names no file: %v", err)
	}
}
