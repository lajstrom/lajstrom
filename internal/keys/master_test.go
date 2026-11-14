package keys

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// testMasterHex is the master key of the tests, built at run time so that no scanner takes a
// literal for a credential.
var testMasterHex = strings.Repeat("0123456789abcdef", 4)

// otherMasterHex is a master key that is not testMasterHex.
var otherMasterHex = strings.Repeat("fedcba9876543210", 4)

// master returns the master key of hex, which must parse.
func master(t testing.TB, hex string) *Master {
	t.Helper()
	m, err := ParseMaster(hex)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseMaster(t *testing.T) {
	for _, tt := range []struct {
		name, in string
		ok       bool
	}{
		{"lowercase", testMasterHex, true},
		{"uppercase", strings.ToUpper(testMasterHex), true},
		{"a line break after it, as from a file", testMasterHex + "\n", true},
		{"spaces around it", "  " + testMasterHex + " ", true},
		{"63 characters", testMasterHex[:63], false},
		{"65 characters", testMasterHex + "0", false},
		{"32 characters, 16 bytes", testMasterHex[:32], false},
		{"not hexadecimal", "g" + testMasterHex[1:], false},
		{"base64 of 32 bytes", "ASNFZ4mrze8BI0VniavN7wEjRWeJq83vASNFZ4mrze8=", false},
		{"empty", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseMaster(tt.in)
			switch {
			case tt.ok && (err != nil || m == nil):
				t.Fatalf("ParseMaster = %v, %v", m, err)
			case !tt.ok && err == nil:
				t.Fatal("ParseMaster succeeded")
			case !tt.ok && !strings.Contains(err.Error(), "64 hexadecimal characters (32 bytes)"):
				t.Errorf("error %q does not say the format", err)
			case !tt.ok && tt.in != "" && strings.Contains(err.Error(), strings.TrimSpace(tt.in)[:8]):
				t.Errorf("error %q shows the value", err)
			}
		})
	}
}

func TestLoadMaster(t *testing.T) {
	env := func(vars map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	}
	for _, tt := range []struct {
		name    string
		envName string
		vars    map[string]string
		noKey   bool     // errors.Is(err, ErrNoMasterKey)
		want    []string // substrings of the error; nil: success
	}{
		{"valid", "LAJSTROM_MASTER_KEY", map[string]string{"LAJSTROM_MASTER_KEY": testMasterHex}, false, nil},
		{"keys.masterKeyEnv empty", "", map[string]string{"LAJSTROM_MASTER_KEY": testMasterHex}, true, []string{"keys.masterKeyEnv is not set"}},
		{"the variable unset", "LAJSTROM_MASTER_KEY", nil, true, []string{"LAJSTROM_MASTER_KEY, which is not set"}},
		{"the variable empty", "LAJSTROM_MASTER_KEY", map[string]string{"LAJSTROM_MASTER_KEY": ""}, false,
			[]string{"keys.masterKeyEnv", "LAJSTROM_MASTER_KEY is empty", "64 hexadecimal characters (32 bytes)", "openssl rand -hex 32"}},
		{"the variable blank", "LAJSTROM_MASTER_KEY", map[string]string{"LAJSTROM_MASTER_KEY": " \n"}, false, []string{"is empty"}},
		{"malformed", "K", map[string]string{"K": "not-a-key-" + testMasterHex[:20]}, false,
			[]string{"keys.masterKeyEnv: the environment variable K", "64 hexadecimal characters (32 bytes)"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, err := LoadMaster(tt.envName, env(tt.vars))
			if tt.want == nil {
				if err != nil || m == nil {
					t.Fatalf("LoadMaster = %v, %v", m, err)
				}
				return
			}
			if err == nil {
				t.Fatal("LoadMaster succeeded")
			}
			if errors.Is(err, ErrNoMasterKey) != tt.noKey {
				t.Errorf("errors.Is(%v, ErrNoMasterKey) = %t, want %t", err, !tt.noKey, tt.noKey)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			if strings.Contains(err.Error(), testMasterHex[:20]) {
				t.Errorf("error %q shows the value", err)
			}
		})
	}
}

// TestMasterRedacted checks that a master key prints as a placeholder in every verb and in a
// log line.
func TestMasterRedacted(t *testing.T) {
	m := master(t, testMasterHex)
	var logged bytes.Buffer
	// Without the time, whose digits could hold the key's first ones (…:10.012380561Z did).
	noTime := func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}
	slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{ReplaceAttr: noTime})).Info("start", "master", m)
	for _, s := range []string{fmt.Sprint(m), fmt.Sprintf("%+v", m), fmt.Sprintf("%#v", m), m.String(),
		fmt.Sprintf("%v", *m), fmt.Sprintf("%+v", struct{ M *Master }{m}), logged.String()} {
		if strings.Contains(s, "0123") || strings.Contains(s, "aead") || !strings.Contains(s, "redacted") {
			t.Errorf("the master key prints as %q", s)
		}
	}
}

// TestSealOpen checks the encryption at rest: the round trip, a fresh nonce per seal, and that
// a wrong master key, another fingerprint as the additional data, a changed byte and a
// truncated ciphertext all fail.
func TestSealOpen(t *testing.T) {
	m := master(t, testMasterHex)
	plain := []byte("private key packets")
	sealed := m.seal("FP1", plain)
	if bytes.Contains(sealed, plain) {
		t.Error("the ciphertext contains the plaintext")
	}
	if again := m.seal("FP1", plain); bytes.Equal(again, sealed) {
		t.Error("two seals of the same key are the same bytes: the nonce is not fresh")
	}
	got, err := m.open("FP1", sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("open = %q, %v", got, err)
	}
	changed := bytes.Clone(sealed)
	changed[len(changed)-1] ^= 1
	for _, tt := range []struct {
		name   string
		m      *Master
		fp     string
		sealed []byte
		want   string
	}{
		{"another master key", master(t, otherMasterHex), "FP1", sealed, "does not decrypt with the master key"},
		{"another fingerprint", m, "FP2", sealed, "does not decrypt with the master key"},
		{"a changed byte", m, "FP1", changed, "does not decrypt with the master key"},
		{"truncated", m, "FP1", sealed[:20], "too short"},
	} {
		if _, err := tt.m.open(tt.fp, tt.sealed); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("open with %s: %v, want an error containing %q", tt.name, err, tt.want)
		}
	}
}

// TestSealSnapshot checks the sealing of a snapshot: the round trip; a wrong master key, other
// additional data, a changed byte and a truncated layer fail; and the snapshot key is not the
// key of the private keys, either way.
func TestSealSnapshot(t *testing.T) {
	m := master(t, testMasterHex)
	plain, ad := []byte("the gzip'd document"), []byte("application/vnd.lajstrom.snapshot.control-plane.v1.sealed")
	sealed := m.SealSnapshot(plain, ad)
	if bytes.Contains(sealed, plain) || bytes.Equal(m.SealSnapshot(plain, ad), sealed) {
		t.Error("the sealed snapshot contains the plaintext, or the nonce is not fresh")
	}
	got, err := m.OpenSnapshot(sealed, ad)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("OpenSnapshot = %q, %v", got, err)
	}
	changed := bytes.Clone(sealed)
	changed[len(changed)/2] ^= 1
	for _, tt := range []struct {
		name   string
		m      *Master
		ad     []byte
		sealed []byte
		want   string
	}{
		{"another master key", master(t, otherMasterHex), ad, sealed, "does not open with the master key"},
		{"other additional data", m, []byte("application/json"), sealed, "does not open with the master key"},
		{"a changed byte", m, ad, changed, "does not open with the master key"},
		{"truncated", m, ad, sealed[:20], "too short"},
	} {
		if _, err := tt.m.OpenSnapshot(tt.sealed, tt.ad); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("OpenSnapshot with %s: %v, want an error containing %q", tt.name, err, tt.want)
		}
	}
	if _, err := m.open(string(ad), sealed); err == nil {
		t.Error("a sealed snapshot opens with the key of the private keys")
	}
	if _, err := m.OpenSnapshot(m.seal(string(ad), plain), ad); err == nil {
		t.Error("a sealed private key opens with the snapshot key")
	}
}
