package format_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

func TestMaxFileSize(t *testing.T) {
	for _, tt := range []struct {
		format, policy string
		want           int64
		err            bool
	}{
		{"raw", "", 8 << 30, false}, // the format's default (#246)
		{"raw", "null", 8 << 30, false},
		{"raw", `{"blobRedirect":"never"}`, 8 << 30, false},
		{"maven", `{"maven":{"sessionTimeout":"1h"}}`, 4 << 30, false},
		{"pypi", "", 4 << 30, false},
		{"apt", "", 8 << 30, false},
		{"npm", "", 512 << 20, false},
		{"oci", "", 16 << 30, false},
		{"oci", `{"maxFileSize":274877906944}`, 256 << 30, false}, // a repo of ML models
		{"rpm", "", 8 << 30, false},                               // a format the table does not name
		{"npm", `{"maxFileSize":1073741824}`, 1 << 30, false},
		{"raw", `{"maxFileSize":1048576}`, 1048576, false},
		{"raw", `{"maxFileSize":0}`, 0, false},
		{"raw", `{"maxFileSize":-1}`, 0, true},
		{"raw", `{"maxFileSize":"1MiB"}`, 0, true},
		{"raw", `{"maxFileSize":1.5}`, 0, true},
	} {
		got, err := format.MaxFileSize(index.Repo{Format: tt.format, Name: "scratch", Policy: []byte(tt.policy)})
		if got != tt.want || (err != nil) != tt.err {
			t.Errorf("MaxFileSize(%s, %s) = %d, %v; want %d, error %v", tt.format, tt.policy, got, err, tt.want, tt.err)
		}
	}
}

// TestLimitBody checks LimitBody with Spool, as the frontends use them: without a policy the
// format's default bound passes a small body; up to the limit it is spooled whole; above it, with a declared
// Content-Length, LimitBody refuses before a read, and without one the spool fails, both 413
// with the repo and the limit named; a policy that cannot be read is 500.
func TestLimitBody(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	repo := index.Repo{Format: "apt", Name: "ubuntu", Policy: []byte(`{"maxFileSize": 10}`)}
	const named = "the file is larger than the maxFileSize of apt/ubuntu, 10 bytes"
	spool := func(repo index.Repo, body string, declared bool) (string, error) {
		t.Helper()
		var r io.Reader = strings.NewReader(body)
		if !declared {
			r = io.MultiReader(r) // httptest.NewRequest sets no Content-Length for it
		}
		req := httptest.NewRequest(http.MethodPut, "/apt/ubuntu/upload", r)
		lim, err := format.LimitBody(httptest.NewRecorder(), req, repo)
		if err != nil {
			return "", err
		}
		sp, err := format.Spool(lim, "upload.deb", "", req.ContentLength)
		if err != nil {
			return "", err
		}
		defer func() { _ = sp.Close() }()
		return readAll(t, sp.File()), nil
	}
	for _, tt := range []struct {
		name     string
		repo     index.Repo
		body     string
		declared bool
		status   int // 0: spooled whole
	}{
		{"no policy", index.Repo{Format: "apt", Name: "ubuntu"}, "0123456789abc", true, 0},
		{"at the limit", repo, "0123456789", true, 0},
		{"at the limit, no length", repo, "0123456789", false, 0},
		{"above, declared", repo, "0123456789a", true, http.StatusRequestEntityTooLarge},
		{"above, no length", repo, "0123456789a", false, http.StatusRequestEntityTooLarge},
		{"unreadable policy", index.Repo{Format: "apt", Name: "ubuntu", Policy: []byte(`{"maxFileSize": "10"}`)}, "x", true,
			http.StatusInternalServerError},
	} {
		got, err := spool(tt.repo, tt.body, tt.declared)
		switch {
		case tt.status == 0 && (err != nil || got != tt.body):
			t.Errorf("%s: spooled %q, %v; want the body", tt.name, got, err)
		case tt.status != 0 && format.StatusOf(err) != tt.status:
			t.Errorf("%s: %v, status %d; want %d", tt.name, err, format.StatusOf(err), tt.status)
		case tt.status == http.StatusRequestEntityTooLarge && !strings.Contains(err.Error(), named):
			t.Errorf("%s: %v does not name the limit", tt.name, err)
		}
	}
}

// TestLimitBodyErrors checks that the limited reader passes the other errors of the body
// through unchanged: io.EOF, so that a reader of it sees the end, and a failure of the client.
func TestLimitBodyErrors(t *testing.T) {
	boom := errors.New("boom")
	req := httptest.NewRequest(http.MethodPut, "/", io.MultiReader(strings.NewReader("abc"), failing{boom}))
	lim, err := format.LimitBody(httptest.NewRecorder(), req, index.Repo{Policy: []byte(`{"maxFileSize": 10}`)})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(lim); string(b) != "abc" || !errors.Is(err, boom) || err.Error() != "boom" {
		t.Errorf("read %q, %v; want abc and boom unchanged", b, err)
	}
	req = httptest.NewRequest(http.MethodPut, "/", strings.NewReader("abc"))
	lim, _ = format.LimitBody(httptest.NewRecorder(), req, index.Repo{Policy: []byte(`{"maxFileSize": 10}`)})
	buf := make([]byte, 8)
	n, _ := lim.Read(buf)
	if _, err := lim.Read(buf[n:]); n != 3 || err != io.EOF {
		t.Errorf("read %d bytes, then %v; want 3 and io.EOF itself", n, err)
	}
}

// failing is a reader that fails with err.
type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }
