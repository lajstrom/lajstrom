package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/deb"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestAdminDebMetadata checks that admin deb metadata prints the metadata layer that Build
// writes for the fixture, byte for byte, without a configuration; and its errors.
func TestAdminDebMetadata(t *testing.T) {
	dir := t.TempDir()
	b, info := testutil.Deb(testutil.DebOptions{Name: "hello-data", Version: "1:2.0-1", Arch: "all"})
	file := filepath.Join(dir, info.Filename)
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	built, err := deb.Plugin{}.Build(t.Context(), index.Repo{Format: deb.Name, Name: "ubuntu"}, format.Upload{
		Files:  []format.UploadedFile{{Name: info.Filename, Size: int64(len(b)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}},
		Params: map[string]string{deb.ParamSuite: "jammy", deb.ParamComponent: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, nil, "admin", "deb", "metadata", file, "--config", filepath.Join(dir, "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(built.Entry.Metadata) {
		t.Errorf("admin deb metadata printed\n%s\nwant the metadata layer of Build\n%s", out, built.Entry.Metadata)
	}
	if !strings.Contains(out, `"filename":"hello-data_2.0-1_all.deb"`) || strings.HasSuffix(out, "\n") {
		t.Errorf("admin deb metadata printed %s", out)
	}

	notDeb := filepath.Join(dir, "not.deb")
	if err := os.WriteFile(notDeb, []byte("not a package"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"admin", "deb", "metadata"}, "accepts 1 arg(s), received 0"},
		{[]string{"admin", "deb", "metadata", filepath.Join(dir, "missing.deb")}, "missing.deb: no such file or directory"},
		{[]string{"admin", "deb", "metadata", notDeb}, notDeb + ": format: invalid: not an ar archive"},
	} {
		if _, _, err := run(t, nil, tt.args...); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%v: error = %v, want it to contain %q", tt.args, err, tt.wantErr)
		}
	}
}
