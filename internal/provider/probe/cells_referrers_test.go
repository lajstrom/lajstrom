package probe

import (
	"testing"

	"github.com/opencontainers/go-digest"
)

func TestReferrersVerdict(t *testing.T) {
	subject := digest.FromString("subject")
	r1, r2 := digest.FromString("r1"), digest.FromString("r2")
	index := ans(200, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`+
		`{"digest":"`+r1.String()+`"},{"digest":"`+r2.String()+`"}]}`, "Content-Type", "application/vnd.oci.image.index.v1+json")
	yes, no := true, false
	cases := []struct {
		name    string
		seen    referrersSeen
		want    string
		subject string
	}{
		{"zot", referrersSeen{api: index, listed: true, filtered: &yes, subject: subject.String(), want: subject},
			"yes, with the `artifactType` filter; no tag schema", "yes"},
		{"nexus oci", referrersSeen{api: index, listed: true, filtered: &yes, written: true, subject: subject.String(), want: subject},
			"yes, with the `artifactType` filter; and it writes the tag schema itself", "yes"},
		{"no filter", referrersSeen{api: index, listed: true, filtered: &no, want: subject},
			"yes, **without** the `artifactType` filter; no tag schema", "no"},
		{"distribution", referrersSeen{api: plain404, want: subject}, "no: a plain 404, the tag schema", "no"},
		{"ghcr", referrersSeen{api: errAns(404, "MANIFEST_UNKNOWN"), want: subject}, "no: 404 `MANIFEST_UNKNOWN`, the tag schema", "no"},
		{"nexus docker", referrersSeen{api: ans(404, "<html/>", "Content-Type", "text/html"), want: subject}, "no: an HTML 404, the tag schema", "no"},
		{"forgejo", referrersSeen{api: ans(404, ""), want: subject}, "no: an empty 404, the tag schema", "no"},
		{"not listed", referrersSeen{api: index, want: subject, subject: r1.String()},
			"**yes, but the referrers pushed are not listed**", "**another digest**: " + r1.String()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := referrersVerdict(c.seen); got != c.want {
				t.Errorf("referrersVerdict = %q, want %q", got, c.want)
			}
			if got := subjectVerdict(c.seen); got != c.subject {
				t.Errorf("subjectVerdict = %q, want %q", got, c.subject)
			}
		})
	}
}

func TestIndexDigests(t *testing.T) {
	if _, ok := indexDigests(ans(200, `{"repositories":[]}`)); ok {
		t.Error("a body without manifests is an index")
	}
	if ds, ok := indexDigests(ans(200, `{"manifests":[]}`)); !ok || len(ds) != 0 {
		t.Errorf("an empty index = %v, %v", ds, ok)
	}
	if _, ok := indexDigests(ans(404, `{"manifests":[]}`)); ok {
		t.Error("a 404 is an index")
	}
}
