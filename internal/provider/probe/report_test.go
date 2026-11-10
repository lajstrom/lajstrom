package probe

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func testReport(name string) *Report {
	r := newReport()
	r.Registry, r.URL, r.Scratch, r.Date, r.Vendor = name, "http://"+name, "lajstrom-probe-0/", "2026-11-10", "unknown"
	c := r.Cell("createOnPush")
	c.Value, c.Measured = "yes, with the first manifest", true
	c.Exchanges = []*Exchange{{Method: "PUT", URL: "/v2/x/manifests/v1", Status: 201, Answer: "Docker-Content-Digest: sha256:" + strings.Repeat("ab", 32)}}
	r.Cell("catalog").Value = "a|b"
	return r
}

func TestWriteMarkdown(t *testing.T) {
	var b bytes.Buffer
	if err := WriteMarkdown(&b, []*Report{testReport("zot"), testReport("distribution")}, false); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"| | zot | distribution |\n| --- | --- | --- |\n",
		"| Create on push | yes, with the first manifest | yes, with the first manifest |\n",
		`| Catalog (` + "`_catalog`" + `) and its limits | a\|b | a\|b |`,
		"| Auth | " + NotMeasured + " | " + NotMeasured + " |",
		"zot: http://zot, scratch prefix `lajstrom-probe-0/`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "```text") {
		t.Error("markdown without verbose has exchanges")
	}
	if rows := strings.Count(out, "\n| "); rows != len(Rows)+1 {
		t.Errorf("%d table lines, want %d", rows, len(Rows)+1)
	}
	b.Reset()
	if err := WriteMarkdown(&b, []*Report{testReport("zot")}, true); err != nil {
		t.Fatal(err)
	}
	if want := "### Create on push\n\nyes, with the first manifest\n\n```text\nPUT /v2/x/manifests/v1"; !strings.Contains(b.String(), want) {
		t.Errorf("verbose markdown lacks %q:\n%s", want, b.String())
	}
	if !strings.Contains(b.String(), "sha256:abababab…") {
		t.Errorf("verbose markdown does not shorten digests:\n%s", b.String())
	}
}

func TestWriteJSON(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		var b bytes.Buffer
		r := testReport("zot")
		if err := WriteJSON(&b, []*Report{r}, verbose); err != nil {
			t.Fatal(err)
		}
		var got []Report
		if err := json.Unmarshal(b.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		c := got[0].Cell("createOnPush")
		if c == nil || c.Value != "yes, with the first manifest" || !c.Measured {
			t.Fatalf("cell createOnPush = %+v", c)
		}
		if has := len(c.Exchanges) > 0; has != verbose {
			t.Errorf("verbose %v: exchanges %d", verbose, len(c.Exchanges))
		}
		if len(r.Cell("createOnPush").Exchanges) != 1 {
			t.Error("WriteJSON changed the report")
		}
	}
}
