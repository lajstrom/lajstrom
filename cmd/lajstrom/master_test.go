package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
)

// noEnv is an empty environment.
func noEnv(string) (string, bool) { return "", false }

// writeKeyFile writes content to a file of mode perm in a new directory and returns its path.
func writeKeyFile(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master-key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil { // WriteFile's mode passes through the umask
		t.Fatal(err)
	}
	return path
}

// TestLoadMasterFile reads the master key from keys.masterKeyFile: the encoding of the
// variable, whitespace around it trimmed; the errors name the file and never its contents.
func TestLoadMasterFile(t *testing.T) {
	malformed := strings.Repeat("zz", 32) // 64 characters, not hexadecimal
	tests := []struct {
		name    string
		content string
		wantErr string // substring; "" means the key loads
	}{
		{"bare", testMasterKey, ""},
		{"trailing newline", testMasterKey + "\n", ""},
		{"CRLF and spaces", "  " + strings.ToUpper(testMasterKey) + " \r\n\n", ""},
		{"empty", "", "is empty; not a master key: the master key is 64 hexadecimal characters"},
		{"only a newline", "\n", "is empty"},
		{"malformed", malformed + "\n", "not a master key: the master key is 64 hexadecimal characters"},
		{"short", testMasterKey[:32], "not a master key"},
		{"two keys", testMasterKey + "\n" + testMasterKey + "\n", "not a master key"},
		{"far too long", strings.Repeat(testMasterKey, 100), "longer than a master key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeKeyFile(t, tt.content, 0o600)
			m, err := loadMaster(config.Keys{MasterKeyFile: path}, noEnv, slog.New(slog.DiscardHandler))
			if tt.wantErr == "" {
				if err != nil || m == nil {
					t.Fatalf("loadMaster: %v, %v", m, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), path) {
				t.Fatalf("error = %v, want it to name %s and contain %q", err, path, tt.wantErr)
			}
			for _, secret := range []string{testMasterKey[:16], malformed[:16]} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error shows the file's contents: %v", err)
				}
			}
		})
	}
}

// TestLoadMasterFileMissing: a file that cannot be read is an error that names it, never the
// signing-off of a missing key, and so is a directory.
func TestLoadMasterFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	dir := t.TempDir()
	for path, want := range map[string]string{missing: "no such file", dir: "is not a regular file"} {
		_, err := loadMaster(config.Keys{MasterKeyFile: path, MasterKeyEnv: "LAJSTROM_MASTER_KEY"},
			noEnv, slog.New(slog.DiscardHandler))
		if err == nil || !strings.Contains(err.Error(), "keys.masterKeyFile") || !strings.Contains(err.Error(), path) ||
			!strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want keys.masterKeyFile, the path and %q", path, err, want)
		}
	}
}

// TestLoadMasterFileMode: a file that the group or others may read or write is used, with a
// warning that names the file and the mode and not the key; 0600 and 0400 give none, and a
// nil logger (the second load of serve) skips the check.
func TestLoadMasterFileMode(t *testing.T) {
	for _, tt := range []struct {
		perm os.FileMode
		warn bool
	}{{0o600, false}, {0o400, false}, {0o640, true}, {0o644, true}, {0o604, true}, {0o660, true}} {
		path := writeKeyFile(t, testMasterKey+"\n", tt.perm)
		var logged bytes.Buffer
		m, err := loadMaster(config.Keys{MasterKeyFile: path}, noEnv, slog.New(slog.NewTextHandler(&logged, nil)))
		if err != nil || m == nil {
			t.Fatalf("%04o: loadMaster: %v", tt.perm, err)
		}
		got := strings.Contains(logged.String(), "level=WARN msg=\"the master key file is open to more than its owner")
		if got != tt.warn {
			t.Errorf("%04o: warned = %v, want %v:\n%s", tt.perm, got, tt.warn, logged.String())
		}
		if tt.warn && (!strings.Contains(logged.String(), path) || !strings.Contains(logged.String(), "mode="+formatPerm(tt.perm))) {
			t.Errorf("%04o: the warning does not name the file and the mode:\n%s", tt.perm, logged.String())
		}
		if strings.Contains(logged.String(), testMasterKey[:16]) {
			t.Errorf("%04o: the log shows the key:\n%s", tt.perm, logged.String())
		}
	}
	path := writeKeyFile(t, testMasterKey, 0o644)
	if _, err := loadMaster(config.Keys{MasterKeyFile: path}, noEnv, nil); err != nil {
		t.Errorf("without a logger: %v", err)
	}
}

func formatPerm(p os.FileMode) string { return fmt.Sprintf("%04o", p) }

// TestAdminKeyMasterKeyFile: the key made under the variable is the key read through the file,
// from keys.masterKeyFile or LAJSTROM_MASTER_KEY_FILE; both sources at once is a configuration
// error of every command, which shows neither value.
func TestAdminKeyMasterKeyFile(t *testing.T) {
	dir := t.TempDir()
	cfg, dsn := writeKeyConfig(t, dir) // keys.masterKeyEnv: LAJSTROM_MASTER_KEY
	keyFile := writeKeyFile(t, testMasterKey+"\n", 0o400)
	export := []string{"admin", "key", "export", "--token"}
	outs := runEnv(t, cfg, map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey},
		[]step{{args: []string{"migrate"}}, {args: export, wantOut: "BEGIN PUBLIC KEY"}})
	viaEnv := runEnv(t, cfg, map[string]string{"LAJSTROM_MASTER_KEY_FILE": keyFile}, []step{{args: export}})
	if viaEnv[0] != outs[1] {
		t.Errorf("through LAJSTROM_MASTER_KEY_FILE: %q, want the key made under the variable %q", viaEnv[0], outs[1])
	}

	withFile := filepath.Join(dir, "with-file.yaml")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"" + dsn + "\"\n" +
		"keys:\n  masterKeyEnv: LAJSTROM_MASTER_KEY\n  masterKeyFile: " + keyFile + "\n"
	if err := os.WriteFile(withFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	viaConfig := runEnv(t, withFile, nil, []step{{args: export}})
	if viaConfig[0] != outs[1] {
		t.Errorf("through keys.masterKeyFile: %q, want %q", viaConfig[0], outs[1])
	}

	const twice = "keys.masterKeyFile: set together with the environment variable LAJSTROM_MASTER_KEY of keys.masterKeyEnv"
	runEnv(t, withFile, map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}, []step{{args: export, wantErr: twice}})
	runEnv(t, cfg, map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey, "LAJSTROM_MASTER_KEY_FILE": keyFile},
		[]step{{args: []string{"migrate"}, wantErr: twice}})
}
