package probe

import (
	"net/http"
	"strings"
	"testing"
)

func TestCreateVerdict(t *testing.T) {
	cases := []struct {
		stage   createStage
		refused string
		want    string
	}{
		{createSession, "", "yes: the first upload session creates the repository"}, // Zot
		{createManifest, "", "yes, with the first manifest"},                        // distribution
		{createBlob, "", "yes: the first blob creates the repository"},
		{createNever, "404 `NAME_UNKNOWN` to the first upload session", "no: 404 `NAME_UNKNOWN` to the first upload session"}, // ECR
	}
	for _, c := range cases {
		if got := createVerdict(c.stage, c.refused); got != c.want {
			t.Errorf("createVerdict(%d) = %q, want %q", c.stage, got, c.want)
		}
	}
}

func TestWhereVerdict(t *testing.T) {
	created := ans(http.StatusCreated, "")
	accepted := ans(http.StatusAccepted, "")
	cases := []struct {
		name   string
		prefix string
		deep   answer
		names  []nameLength
		want   string
	}{
		{"distribution", "", created, []nameLength{{"255", accepted}, {"256", errAns(400, "NAME_INVALID")}},
			"anywhere; at any depth, inside another repository too; at most 255 characters (256: 400 `NAME_INVALID`)"},
		{"zot", "", created, []nameLength{{"255", accepted}, {"256", accepted}, {"component", ans(500, "")}},
			"anywhere; at any depth, inside another repository too; a name component of more than 255 bytes is an empty 500"},
		{"gitea", "lajstrom-it/", created, []nameLength{{"255", accepted}, {"256", accepted}, {"component", accepted}},
			"below `lajstrom-it/`; outside it not tried; at any depth, inside another repository too; no length limit met: 256 characters, a component of 256 bytes"},
		{"depth refused", "", errAns(400, "NAME_INVALID"), []nameLength{{"255", errAns(400, "NAME_INVALID")}, {"256", errAns(400, "NAME_INVALID")}},
			"anywhere; eight levels deep: 400 `NAME_INVALID`; 255 characters refused: 400 `NAME_INVALID`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := whereVerdict(c.prefix, c.deep, c.names); got != c.want {
				t.Errorf("whereVerdict = %q\nwant           %q", got, c.want)
			}
		})
	}
}

func TestLongName(t *testing.T) {
	r := &run{scratch: "lajstrom-it/lajstrom-probe-0011aabb/"}
	for _, n := range []int{255, 256} {
		name := r.longName(n)
		if len(name) != n || !strings.HasPrefix(name, r.scratch) {
			t.Errorf("longName(%d) = %d characters: %s", n, len(name), name)
		}
		for c := range strings.SplitSeq(name, "/") {
			if c == "" || len(c) > 64 {
				t.Errorf("longName(%d) has a component of %d characters", n, len(c))
			}
		}
	}
}

func TestRepositoryDeleteVerdict(t *testing.T) {
	cases := []struct {
		name            string
		catalog, listed bool
		tags            answer
		want            string
	}{
		{"distribution on a filesystem", true, true, ans(200, `{"tags":[]}`), "no `/v2` API; an emptied repository stays in `_catalog`, its tag list 200"},
		{"distribution on S3", true, false, ans(200, `{"tags":[]}`), "no `/v2` API; it leaves `_catalog` with its last manifest, its tag list 200"},
		{"zot with htpasswd", true, false, errAns(404, "NAME_UNKNOWN"), "no `/v2` API; it leaves `_catalog` with its last manifest, its tag list 404 `NAME_UNKNOWN`"},
		{"harbor robot", false, false, errAns(404, "NOT_FOUND"), "no `/v2` API; no catalog to look in, its tag list 404 `NOT_FOUND`"},
	}
	for _, c := range cases {
		if got := repositoryDeleteVerdict(c.catalog, c.listed, c.tags); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
