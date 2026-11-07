package format_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
)

// spoolFiles returns the spool files in dir, where TMPDIR points during the test.
func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "lajstrom-upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// readAll opens f and reads it to the end.
func readAll(t *testing.T, f format.UploadedFile) string {
	t.Helper()
	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSpool checks that the body is copied, that the file reopens from the start as often as
// asked, also with two readers at once, and that Close removes it and can be repeated.
func TestSpool(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	for _, size := range []int64{11, -1} {
		s, err := format.Spool(strings.NewReader("hello world"), "docs/a.txt", "text/plain", size)
		if err != nil {
			t.Fatal(err)
		}
		f := s.File()
		if f.Name != "docs/a.txt" || f.Size != 11 || f.ContentType != "text/plain" {
			t.Errorf("File() = %+v", f)
		}
		first, _ := f.Open()
		if got := readAll(t, f); got != "hello world" {
			t.Errorf("read = %q", got)
		}
		if b, _ := io.ReadAll(first); string(b) != "hello world" {
			t.Errorf("concurrent reader read %q", b)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
	}
	if left := spoolFiles(t, dir); len(left) != 0 {
		t.Errorf("spool files left after Close: %v", left)
	}
}

// failingReader returns its error after some bytes.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// TestSpoolErrors checks that a body that is longer than MaxBytesReader allows surfaces as
// *http.MaxBytesError (413), that a body shorter than declared is ErrInvalid (400), that a read
// error is returned wrapped, and that none of them leaves a file.
func TestSpoolErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	broken := errors.New("connection reset")
	tests := []struct {
		name   string
		body   func(w http.ResponseWriter) io.Reader
		size   int64
		want   error
		status int
	}{
		{name: "too large", size: -1, status: http.StatusRequestEntityTooLarge,
			body: func(w http.ResponseWriter) io.Reader {
				return http.MaxBytesReader(w, io.NopCloser(strings.NewReader("0123456789")), 4)
			}},
		{name: "shorter than declared", size: 20, want: format.ErrInvalid, status: http.StatusBadRequest,
			body: func(http.ResponseWriter) io.Reader { return strings.NewReader("0123456789") }},
		{name: "read error", size: -1, want: broken, status: http.StatusInternalServerError,
			body: func(http.ResponseWriter) io.Reader { return failingReader{broken} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := format.Spool(tt.body(httptest.NewRecorder()), "a", "", tt.size)
			if s != nil || err == nil {
				t.Fatalf("Spool = %v, %v; want an error", s, err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("error %v does not wrap %v", err, tt.want)
			}
			if got := format.StatusOf(err); got != tt.status {
				t.Errorf("StatusOf(%v) = %d, want %d", err, got, tt.status)
			}
			if left := spoolFiles(t, dir); len(left) != 0 {
				t.Errorf("spool files left: %v", left)
			}
		})
	}
}

// TestSpoolNoTempDir checks that a temporary directory that does not exist is an error.
func TestSpoolNoTempDir(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if _, err := format.Spool(strings.NewReader("x"), "a", "", 1); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Spool without a temporary directory: %v, want ErrNotExist", err)
	}
}
