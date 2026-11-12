package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/rpm"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestAdminRpmMetadata checks that admin rpm metadata prints the metadata layer that Build
// writes for the fixture, byte for byte, without a configuration; and its errors.
func TestAdminRpmMetadata(t *testing.T) {
	dir := t.TempDir()
	b, info := testutil.RPM(testutil.RPMOptions{Name: "hello-data", Epoch: "1", Arch: "noarch"})
	file := filepath.Join(dir, info.Filename)
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	built, err := rpm.Plugin{}.Build(t.Context(), index.Repo{Format: rpm.Name, Name: "el9"}, format.Upload{
		Files: []format.UploadedFile{{Name: info.Filename, Size: int64(len(b)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, nil, "admin", "rpm", "metadata", file, "--config", filepath.Join(dir, "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(built.Entry.Metadata) {
		t.Errorf("admin rpm metadata printed\n%s\nwant the metadata layer of Build\n%s", out, built.Entry.Metadata)
	}
	if !strings.Contains(out, `"filename":"hello-data-1.0.0-1.el9.noarch.rpm"`) || strings.HasSuffix(out, "\n") {
		t.Errorf("admin rpm metadata printed %s", out)
	}

	notRPM := filepath.Join(dir, "not.rpm")
	if err := os.WriteFile(notRPM, []byte("not a package"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"admin", "rpm", "metadata"}, "accepts 1 arg(s), received 0"},
		{[]string{"admin", "rpm", "metadata", filepath.Join(dir, "missing.rpm")}, "missing.rpm: no such file or directory"},
		{[]string{"admin", "rpm", "metadata", notRPM}, notRPM + ": format: invalid: the file has 13 bytes, fewer than the lead"},
	} {
		if _, _, err := run(t, nil, tt.args...); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%v: error = %v, want it to contain %q", tt.args, err, tt.wantErr)
		}
	}
}
