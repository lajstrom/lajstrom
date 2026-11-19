package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// keyVolume makes in a new directory what a Kubernetes Secret or a Secrets Store CSI volume
// mounted whole makes of files (name to content): the files in a timestamped directory of
// mode perm, ..data a link to it, and each name a link through ..data. It returns the directory.
func keyVolume(t *testing.T, files map[string]string, perm os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	stamped := filepath.Join(dir, "..2026_11_19_10_00_00.123456789")
	if err := os.Mkdir(stamped, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(stamped, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Base(stamped), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// heldIDsOf returns the ids and the sources of m, "id source" each.
func heldIDsOf(m *keys.Master) []string {
	var out []string
	for _, k := range m.Held() {
		out = append(out, k.ID+" "+k.Source)
	}
	return out
}

// TestLoadMasterDir: keys.masterKeyDir holds a key per file, read through the links of a Secret
// or CSI volume, the entries whose name starts with a dot ignored; its keys join the one of the
// variable or of keys.masterKeyFile, a key in two sources once; the variable unset leaves the
// directory's keys. A file that the group or others may read is used, with a warning naming the
// file and never the key.
func TestLoadMasterDir(t *testing.T) {
	a, b, c := masterID(t, testMasterKey), masterID(t, otherMasterKey), masterID(t, thirdMasterKey)
	dir := keyVolume(t, map[string]string{"new": otherMasterKey + "\n", "old": testMasterKey + "\n"}, 0o400)
	if err := os.WriteFile(filepath.Join(dir, ".not-a-key"), []byte("anything"), 0o600); err != nil {
		t.Fatal(err)
	}
	withEnv := func(v string) func(string) (string, bool) {
		return func(k string) (string, bool) { return v, k == "LAJSTROM_MASTER_KEY" && v != "" }
	}
	file := writeKeyFile(t, thirdMasterKey, 0o600)
	for _, tt := range []struct {
		name string
		keys config.Keys
		env  string
		want []string
	}{
		{"the directory alone", config.Keys{MasterKeyDir: dir}, "", []string{a + " " + filepath.Join(dir, "old"), b + " " + filepath.Join(dir, "new")}},
		{"the variable unset", config.Keys{MasterKeyEnv: "LAJSTROM_MASTER_KEY", MasterKeyDir: dir}, "",
			[]string{a + " " + filepath.Join(dir, "old"), b + " " + filepath.Join(dir, "new")}},
		{"the variable and the directory", config.Keys{MasterKeyEnv: "LAJSTROM_MASTER_KEY", MasterKeyDir: dir}, thirdMasterKey,
			[]string{a + " " + filepath.Join(dir, "old"), b + " " + filepath.Join(dir, "new"), c + " LAJSTROM_MASTER_KEY"}},
		{"a key in two sources", config.Keys{MasterKeyEnv: "LAJSTROM_MASTER_KEY", MasterKeyDir: dir}, testMasterKey,
			[]string{a + " LAJSTROM_MASTER_KEY", b + " " + filepath.Join(dir, "new")}},
		{"the file and the directory", config.Keys{MasterKeyFile: file, MasterKeyDir: dir}, "",
			[]string{a + " " + filepath.Join(dir, "old"), b + " " + filepath.Join(dir, "new"), c + " " + file}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logged bytes.Buffer
			m, err := loadMasters(tt.keys, withEnv(tt.env), slog.New(slog.NewTextHandler(&logged, nil)))
			if err != nil {
				t.Fatal(err)
			}
			got := heldIDsOf(m)
			want := slices.Clone(tt.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("held %q, want %q", got, want)
			}
			if logged.Len() > 0 {
				t.Errorf("a warning for files of mode 0400:\n%s", logged.String())
			}
		})
	}

	open := keyVolume(t, map[string]string{"k": testMasterKey}, 0o644)
	var logged bytes.Buffer
	if _, err := loadMasters(config.Keys{MasterKeyDir: open}, noEnv, slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "level=WARN msg=\"the master key file is open to more than its owner") ||
		!strings.Contains(logged.String(), filepath.Join(open, "k")) || !strings.Contains(logged.String(), "mode=0644") ||
		strings.Contains(logged.String(), testMasterKey[:16]) {
		t.Errorf("the warning of a key of mode 0644 through a link:\n%s", logged.String())
	}
}

// TestLoadMasterDirErrors: an entry that is neither a file nor a link to one, a link to nothing,
// a file that is not a key, a directory without a key or that does not exist, and more than 8 keys
// are errors that name the entry or the directory, never a key; none of them is "no master key".
func TestLoadMasterDirErrors(t *testing.T) {
	malformed := strings.Repeat("zz", 32)
	withSub := keyVolume(t, map[string]string{"k": testMasterKey}, 0o400)
	if err := os.Mkdir(filepath.Join(withSub, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	dangling := keyVolume(t, map[string]string{"k": testMasterKey}, 0o400)
	if err := os.Symlink("nowhere", filepath.Join(dangling, "gone")); err != nil {
		t.Fatal(err)
	}
	toDir := keyVolume(t, map[string]string{"k": testMasterKey}, 0o400)
	if err := os.Symlink("..data", filepath.Join(toDir, "data")); err != nil {
		t.Fatal(err)
	}
	bad := keyVolume(t, map[string]string{"k": testMasterKey, "bad": malformed}, 0o400)
	empty := keyVolume(t, map[string]string{}, 0o400)
	many := map[string]string{}
	for i := range keys.MaxMasterKeys + 1 {
		many[fmt.Sprint("k", i)] = fmt.Sprintf("%064x", i+1)
	}
	tooMany := keyVolume(t, many, 0o400)
	for _, tt := range []struct {
		dir, want string
	}{
		{withSub, filepath.Join(withSub, "sub") + " is not a regular file, nor a link to one"},
		{toDir, filepath.Join(toDir, "data") + " is not a regular file, nor a link to one"},
		{dangling, filepath.Join(dangling, "gone")},
		{bad, "keys.masterKeyDir: the file " + filepath.Join(bad, "bad") + ": not a master key"},
		{empty, "keys.masterKeyDir: the directory " + empty + " holds no key"},
		{filepath.Join(empty, "missing"), "keys.masterKeyDir: open " + filepath.Join(empty, "missing")},
		{tooMany, "keys.masterKeyDir " + tooMany + ": the sources of the master keys hold 9 keys, more than 8"},
	} {
		_, err := loadMasters(config.Keys{MasterKeyEnv: "LAJSTROM_MASTER_KEY", MasterKeyDir: tt.dir}, noEnv, nil)
		if err == nil || !strings.Contains(err.Error(), tt.want) || errors.Is(err, keys.ErrNoMasterKey) {
			t.Errorf("%s: %v, want an error containing %q", tt.dir, err, tt.want)
		}
		if err != nil && (strings.Contains(err.Error(), testMasterKey[:16]) || strings.Contains(err.Error(), malformed[:16])) {
			t.Errorf("%s: the error shows a key: %v", tt.dir, err)
		}
	}
}

// TestMasterDirSeveral: a process of a directory of two keys on a database that names none stops,
// naming the rewrap and the ids; once the index names one of them, it starts and seals with it.
func TestMasterDirSeveral(t *testing.T) {
	cfg, dsn := writeKeyConfig(t, t.TempDir())
	dir := keyVolume(t, map[string]string{"old": testMasterKey, "new": otherMasterKey}, 0o400)
	appendConfig(t, cfg, "  masterKeyDir: "+dir+"\n")
	runEnv(t, cfg, nil, []step{
		{args: []string{"migrate"}},
		{args: []string{"admin", "key", "export", "--token"}, wantErr: "no master key seals yet: name one with lajstrom admin key rewrap --to <id>"},
	})
	switchSealing(t, dsn, "", masterID(t, otherMasterKey))
	runEnv(t, cfg, nil, []step{{args: []string{"admin", "key", "export", "--token"}, wantOut: "BEGIN PUBLIC KEY"}})
	st, err := sqlite.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ks, err := st.ListKeys(t.Context(), nil)
	if err != nil || len(ks) != 1 {
		t.Fatalf("the token key: %v, %v", ks, err)
	}
	m, err := loadMasters(config.Keys{MasterKeyDir: dir}, noEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, by, err := m.OpenKey(ks[0].Fingerprint, ks[0].PrivateEnc); err != nil || by != masterID(t, otherMasterKey) {
		t.Errorf("the token key is sealed under %s (%v), want the key the index names", by, err)
	}
	if k, err := st.GetSealingKey(t.Context()); err != nil || k.Source != index.SealingRewrap {
		t.Errorf("the sealing row: %+v, %v", k, err)
	}
}
