package npm

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// readAll returns the decoded data of the attachment.
func (a *attachment) readAll(t *testing.T) []byte {
	t.Helper()
	rc, err := a.file.File().Open()
	must(t, err)
	b, err := io.ReadAll(rc)
	must(t, err)
	return b
}

// TestReadBodyCaptured reads the publish bodies of npm 10 and 11, pnpm 10 and yarn 4, captured
// from the real clients (an unscoped and a scoped package each; yarn puts _attachments first and
// the readme at the top): the members are what encoding/json reads, the one attachment decodes
// to a tarball of its length whose sha512 is the version's dist.integrity, and whose
// package.json has the body's name.
func TestReadBodyCaptured(t *testing.T) {
	files, err := filepath.Glob("testdata/publish/*.json")
	must(t, err)
	if len(files) != 8 {
		t.Fatalf("%d captured bodies, want 8", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // a captured body of this package's testdata
		must(t, err)
		b, err := readBody(bytes.NewReader(raw), -1, "npm/test")
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		var whole map[string]json.RawMessage
		must(t, json.Unmarshal(raw, &whole))
		delete(whole, keyAttachments)
		for k, v := range whole {
			if !sameValue(v, b.fields[k]) || b.fields[k] == nil {
				t.Errorf("%s: %s = %s, encoding/json %s", f, k, b.fields[k], v)
			}
		}
		var body struct {
			Name     string
			Versions map[string]struct{ Dist struct{ Integrity string } }
		}
		must(t, json.Unmarshal(raw, &body))
		a := b.attachments[0]
		data := a.readAll(t)
		var length int
		must(t, json.Unmarshal(a.length, &length))
		tb, err := ReadTarball(bytes.NewReader(data))
		must(t, err)
		if len(b.attachments) != 1 || length != len(data) || tb.PackageJSON.Name != body.Name ||
			"sha512-"+base64.StdEncoding.EncodeToString(a.sha512) != body.Versions["1.0.0"].Dist.Integrity {
			t.Errorf("%s: attachment %q of %d bytes, length %d, package.json %s", f, a.name, len(data), length, tb.PackageJSON.Name)
		}
		must(t, b.Close())
	}
}

// TestReadBodyEscapes reads data with JSON escapes, \/ among them, which some encoders write,
// and an attachment before the other members.
func TestReadBodyEscapes(t *testing.T) {
	want := []byte{0xff, 0xff, 0xff, 'A', 'B', 'C'}
	body := `  {"_attachments" : {"x.tgz": {"length": 6, "data": "\/\/\/\/QUJD\n", "content_type": "application/octet-stream"}} ,` +
		` "name": "x", "versions": {"1.0.0": {"a": [1, {"b": "\"}"}], "c": null, "d": -1.5e3, "e": true}}, "empty": {}}` + "\n"
	b, err := readBody(strings.NewReader(body), 6, "npm/test")
	must(t, err)
	defer func() { _ = b.Close() }()
	if got := b.attachments[0].readAll(t); !bytes.Equal(got, want) {
		t.Errorf("data = %q, want %q", got, want)
	}
	if string(b.fields["versions"]) != `{"1.0.0": {"a": [1, {"b": "\"}"}], "c": null, "d": -1.5e3, "e": true}}` || string(b.fields["empty"]) != "{}" {
		t.Errorf("fields %s", b.fields)
	}
}

// TestReadBodyRefuses is the bodies readBody refuses: 400, or 413 over a bound.
func TestReadBodyRefuses(t *testing.T) {
	att := func(data string) string { return `{"_attachments": {"x.tgz": {"data": "` + data + `"}}}` }
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"an array", `[]`, 400},
		{"empty", ``, 400},
		{"cut", `{"name": "x"`, 400},
		{"cut in a value", `{"name": "x`, 400},
		{"more after the object", `{"name": "x"} {}`, 400},
		{"a name without quotes", `{name: "x"}`, 400},
		{"no colon", `{"name" "x"}`, 400},
		{"no comma", `{"name": "x" "a": 1}`, 400},
		{"a trailing comma", `{"name": "x", "a": 1,}`, 400},
		{"cut after a comma", `{"name": "x", "a": 1,`, 400},
		{"a value that is no JSON", `{"name": tru}`, 400},
		{"a closing bracket too many", `{"name": ]}`, 400},
		{"two attachments", `{"_attachments": {"a.tgz": {"data": "QQ=="}, "b.tgz": {"data": "QQ=="}}}`, 400},
		{"_attachments twice", `{"_attachments": {}, "_attachments": {}}`, 400},
		{"data twice", `{"_attachments": {"a.tgz": {"data": "QQ==", "data": "QQ=="}}}`, 400},
		{"data that is no string", `{"_attachments": {"a.tgz": {"data": 1}}}`, 400},
		{"data that is not base64", att("!!!!"), 400},
		{"a control character in the data", att("QQ\x01=="), 400},
		{"an escape outside ASCII", att(`QQé=`), 400},
		{"data cut", `{"_attachments": {"a.tgz": {"data": "QUFB`, 400},
		{"data over maxFileSize", att("QUFBQQ=="), 413},
		{"a body over 4 MiB", `{"readme": "` + strings.Repeat("x", maxBody) + `"}`, 413},
	} {
		b, err := readBody(strings.NewReader(tc.body), 3, "npm/test")
		if got := format.StatusOf(err); got != tc.status {
			t.Errorf("%s: %d %v, want %d", tc.name, got, err, tc.status)
		}
		if b != nil {
			t.Errorf("%s: a body", tc.name)
		}
	}
}

// repeated is a reader of n bytes of c.
type repeated struct {
	c byte
	n int64
}

func (r *repeated) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	k := min(int64(len(p)), r.n)
	for i := range k {
		p[i] = r.c
	}
	r.n -= k
	return int(k), nil
}

// TestReadBodyBounded is the bound of #305: the attachment of a 100 MiB tarball, 133 MiB of
// base64, read as it streams. What readBody allocates does not grow with it: the data goes to
// the spool file.
func TestReadBodyBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("decodes 100 MiB")
	}
	const size = 100 << 20
	body := func() io.Reader {
		return io.MultiReader(strings.NewReader(`{"name": "big", "_attachments": {"big-1.0.0.tgz": {"length": 104857600, "data": "`),
			&repeated{c: 'A', n: size / 3 * 4}, strings.NewReader(`AAAA"}}}`))
	}
	var b *publishBody
	var err error
	testutil.CheckAllocated(t, "readBody of 100 MiB", testutil.AllocSlack+1<<20, func() {
		if b != nil {
			_ = b.Close()
		}
		b, err = readBody(body(), -1, "npm/test")
	})
	must(t, err)
	defer func() { _ = b.Close() }()
	zeros := sha512.New()
	_, _ = io.Copy(zeros, &repeated{c: 0, n: size / 3 * 3})
	_, _ = zeros.Write([]byte{0, 0, 0})
	if st := b.attachments[0].file.File(); st.Size != size/3*3+3 || !bytes.Equal(b.attachments[0].sha512, zeros.Sum(nil)) {
		t.Errorf("the attachment has %d bytes, sha512 %x", st.Size, b.attachments[0].sha512)
	}
	var tooLarge *http.MaxBytesError
	if _, err := readBody(body(), 1<<20, "npm/test"); format.StatusOf(err) != 413 || !strings.Contains(err.Error(), "maxFileSize of npm/test") ||
		!errors.As(err, &tooLarge) {
		t.Errorf("over maxFileSize: %v", err)
	}
}
