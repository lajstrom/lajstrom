package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// rbacFixture is a fixture with the real evaluator over its index, the server-wide anonymous
// read on, and the users of names bound as bindings says: "<user> <role> <mask>".
func rbacFixture(t *testing.T, bindings ...[3]string) *fixture {
	t.Helper()
	f := newFixture(t, Options{})
	for _, b := range bindings {
		if _, err := f.st.GetUser(context.Background(), b[0]); err != nil {
			f.addUsers(t, b[0])
		}
		must(t, f.st.CreateBinding(context.Background(), &index.Binding{Principal: "user:" + f.uidOf(t, b[0]), Role: b[1], RepoMask: b[2]}))
	}
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log,
		Auth: auth.NewRBAC(auth.RBACOptions{Index: f.st, AnonymousRead: true, Log: f.repos.Log})}))
	return f
}

// TestRepoPermissions: a repo's permissions are what the evaluator allows the request on it,
// the scopes of its credential included (docs/spec/rbac.md §3), in the order read, write,
// delete, manage, in GET of the repo and in the list alike; anonymous reads a repo with
// anonymous read and nothing else.
func TestRepoPermissions(t *testing.T) {
	f := rbacFixture(t, [3]string{"root", auth.RoleAdmin, "*/*"}, [3]string{"erin", auth.RoleMaintainer, "raw/*"},
		[3]string{"wanda", auth.RoleWriter, "raw/scratch"}, [3]string{"alice", auth.RoleReader, "raw/*"})
	f.create(t, "raw", "scratch")
	f.create(t, "raw", "other")
	f.create(t, "pypi", "open")
	if _, err := f.repos.Update(context.Background(), "pypi", "open", UpdateRequest{AnonymousRead: new(true)}, Origin{}); err != nil {
		t.Fatal(err)
	}
	user := func(name string) auth.Principal { return auth.Principal{Kind: auth.KindUser, Name: name} }
	password := auth.Credential{Kind: auth.CredPassword}
	tok := auth.Principal{Kind: auth.KindToken, Name: "7"}
	for _, tt := range []struct {
		name string
		p    auth.Principal
		c    auth.Credential
		want map[string]string // repo: its permissions; the list has exactly these repos
	}{
		{"admin", user("root"), password, map[string]string{"pypi/open": "read write delete manage",
			"raw/other": "read write delete manage", "raw/scratch": "read write delete manage"}},
		{"maintainer of raw/*", user("erin"), password, map[string]string{"pypi/open": "read",
			"raw/other": "read write delete manage", "raw/scratch": "read write delete manage"}},
		{"a read-only personal access token of the maintainer", user("erin"),
			auth.Credential{Kind: auth.CredPAT, TokenID: 3, Scopes: []string{"read"}},
			map[string]string{"pypi/open": "read", "raw/other": "read", "raw/scratch": "read"}},
		{"writer of raw/scratch", user("wanda"), password, map[string]string{"pypi/open": "read", "raw/scratch": "read write"}},
		{"reader of raw/*", user("alice"), password, map[string]string{"pypi/open": "read", "raw/other": "read", "raw/scratch": "read"}},
		{"a deploy token of raw/scratch", tok, auth.Credential{Kind: auth.CredDeploy, TokenID: 7, Repo: "raw/scratch",
			Scopes: []string{"read", "write"}}, map[string]string{"raw/scratch": "read write"}},
		{"anonymous", auth.Anonymous, auth.Credential{}, map[string]string{"pypi/open": "read"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var list RepoList
			decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/repos", "", tt.p, tt.c), http.StatusOK, &list)
			got := map[string]string{}
			for _, r := range list.Repos {
				got[r.Format+"/"+r.Name] = actions(r.Permissions)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("the list's permissions = %v, want %v", got, tt.want)
			}
			for id, want := range tt.want {
				var one Repo
				decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/repos/"+id, "", tt.p, tt.c), http.StatusOK, &one)
				if got := actions(one.Permissions); got != want {
					t.Errorf("GET %s: permissions %q, want %q", id, got, want)
				}
			}
		})
	}
}

// actions joins the actions of a permissions list with spaces.
func actions(as []auth.Action) string {
	var b bytes.Buffer
	for i, a := range as {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(string(a))
	}
	return b.String()
}

// TestRepoStats: a repo's stats count its packages, distinct versions, manifests, files and
// their sizes, and give its newest publish time, in the list and in GET of the repo; a repo
// without content has zeros and no time; a group and an oci repo have no stats.
func TestRepoStats(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	full := f.create(t, "raw", "full")
	f.create(t, "raw", "empty")
	for _, r := range []index.Repo{
		{Format: "oci", Name: "hub", Registry: "local", Prefix: "oci/hub", Kind: index.KindHosted},
		{Format: "raw", Name: "all", Prefix: "raw/all", Kind: index.KindGroup, Group: []byte(`{"merge":[]}`)},
	} {
		must(t, f.st.CreateRepo(ctx, &r))
	}
	published := time.Date(2026, 11, 14, 9, 12, 3, 0, time.UTC)
	for i, v := range []struct {
		pkg, version, variant string
		sizes                 []int64
	}{{"a", "1.0", "", []int64{10, 20}}, {"a", "2.0", "x", []int64{5}}, {"a", "2.0", "y", []int64{5}}, {"b", "1.0", "", nil}} {
		pkg := index.Package{RepoID: full.ID, Name: v.pkg, NameOCI: v.pkg}
		must(t, f.st.UpsertPackage(ctx, &pkg))
		row := index.Version{PackageID: pkg.ID, Version: v.version, Variant: v.variant, Tag: v.version + v.variant,
			Digest: digest.FromString(fmt.Sprint(i)), ArtifactType: "application/vnd.lajstrom.raw.v1",
			CreatedAt: published.Add(-time.Duration(i) * time.Hour)}
		must(t, f.st.UpsertVersion(ctx, &row))
		var files []index.File
		for j, size := range v.sizes {
			files = append(files, index.File{Name: fmt.Sprint(j), MediaType: "application/octet-stream",
				Digest: digest.FromString(fmt.Sprint(i, j)), Size: size})
		}
		must(t, f.st.ReplaceFiles(ctx, row.ID, files))
	}

	var list RepoList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos", "", admin), http.StatusOK, &list)
	got := map[string]string{}
	for _, r := range list.Repos {
		got[r.Format+"/"+r.Name] = mustJSON(t, r.Stats)
	}
	want := map[string]string{
		"oci/hub":   "null",
		"raw/all":   "null",
		"raw/empty": `{"packages":0,"versions":0,"manifests":0,"files":0,"size":0,"lastPublishedAt":null}`,
		"raw/full":  `{"packages":2,"versions":3,"manifests":4,"files":4,"size":40,"lastPublishedAt":"2026-11-14T09:12:03Z"}`,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the list's stats = %v, want %v", got, want)
	}
	var one map[string]any
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/raw/full", "", admin), http.StatusOK, &one)
	wantJSON(t, "the stats of raw/full", one["stats"], want["raw/full"])
	if _, ok := one["signingKeys"]; ok {
		t.Errorf("raw/full has signingKeys: %v", one)
	}
	var group map[string]any
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/raw/all", "", admin), http.StatusOK, &group)
	if _, ok := group["stats"]; ok {
		t.Errorf("the group raw/all has stats: %v", group)
	}
}

// TestRepoSigningKeys: an apt or rpm repo answers its keys' public facts, the active key first,
// then the retired ones, the most recently retired first; [] before its first key; never a
// private part.
func TestRepoSigningKeys(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	apt := index.Repo{Format: "apt", Name: "ubuntu", Registry: "local", Prefix: "apt/ubuntu", Kind: index.KindHosted}
	rpm := index.Repo{Format: "rpm", Name: "el9", Registry: "local", Prefix: "rpm/el9", Kind: index.KindHosted}
	for _, r := range []*index.Repo{&apt, &rpm} {
		must(t, f.st.CreateRepo(ctx, r))
	}
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for i, k := range []struct {
		algo    packet.PublicKeyAlgorithm
		retired *time.Time
	}{{packet.PubKeyAlgoEdDSA, new(t0.Add(48 * time.Hour))}, {packet.PubKeyAlgoRSA, new(t0.Add(96 * time.Hour))}, {packet.PubKeyAlgoEdDSA, nil}} {
		e, err := openpgp.NewEntity("Lajstrom apt/ubuntu", "", "", &packet.Config{Algorithm: k.algo, RSABits: 2048})
		must(t, err)
		var pub bytes.Buffer
		must(t, e.PrimaryKey.Serialize(&pub))
		row := index.Key{RepoID: &apt.ID, Kind: "gpg", Public: pub.Bytes(), PrivateEnc: []byte("sealed"),
			Fingerprint: fmt.Sprintf("%X", e.PrimaryKey.Fingerprint), CreatedAt: t0.Add(time.Duration(i) * 24 * time.Hour)}
		must(t, f.st.CreateKey(ctx, &row))
		if k.retired != nil {
			must(t, f.st.RetireKey(ctx, row.ID, *k.retired))
		}
	}
	var got Repo
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/apt/ubuntu", "", admin), http.StatusOK, &got)
	if got.SigningKeys == nil || len(*got.SigningKeys) != 3 {
		t.Fatalf("signingKeys = %v, want 3", got.SigningKeys)
	}
	ks := *got.SigningKeys
	order := fmt.Sprintf("%s %v | %s %v | %s %v", ks[0].Algorithm, ks[0].RetiredAt, ks[1].Algorithm, ks[1].RetiredAt.Format(time.RFC3339),
		ks[2].Algorithm, ks[2].RetiredAt.Format(time.RFC3339))
	if want := "ed25519 <nil> | rsa2048 2026-11-11T12:00:00Z | ed25519 2026-11-09T12:00:00Z"; order != want {
		t.Errorf("signingKeys in the order %q, want %q", order, want)
	}
	if !ks[0].CreatedAt.Equal(t0.Add(48*time.Hour)) || len(ks[0].Fingerprint) != 40 {
		t.Errorf("the active key = %+v", ks[0])
	}
	var raw map[string]any
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/rpm/el9", "", admin), http.StatusOK, &raw)
	wantJSON(t, "the signingKeys of rpm/el9, which has none yet", raw["signingKeys"], `[]`)
	if body := f.do(http.MethodGet, "/api/v1/repos", "", admin).Body.String(); bytes.Contains([]byte(body), []byte("sealed")) {
		t.Errorf("the list holds a private part: %s", body)
	}
}
