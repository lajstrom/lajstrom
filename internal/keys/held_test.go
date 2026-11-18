package keys

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
)

// thirdMasterHex is a third master key of the tests.
var thirdMasterHex = strings.Repeat("00112233445566778899aabbccddeeff", 2)

// held returns the Master of the keys of hexes, from sources named src0, src1, …, none sealing.
func held(t testing.TB, hexes ...string) *Master {
	t.Helper()
	var srcs []MasterSource
	for i, h := range hexes {
		srcs = append(srcs, MasterSource{Source: fmt.Sprintf("src%d", i), Text: h})
	}
	m, err := HoldMasters(srcs)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// id returns the id of the key of hex.
func id(t testing.TB, hex string) string {
	t.Helper()
	id, err := MasterID(hex)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestMasterID: a key's id is 16 lowercase hexadecimal digits, the same for the key in either
// case and with spaces around it, another for another key, and shows nothing of the key: it is
// the HKDF of the spec, pinned here so that ids compare across releases.
func TestMasterID(t *testing.T) {
	a := id(t, testMasterHex)
	if len(a) != 16 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("id %q is not 16 lowercase hexadecimal digits", a)
	}
	if a != id(t, " "+strings.ToUpper(testMasterHex)+"\n") {
		t.Error("the id of the key in uppercase, with spaces around it, is another")
	}
	if a == id(t, otherMasterHex) {
		t.Error("two keys have one id")
	}
	if strings.Contains(testMasterHex, a) || strings.Contains(a, testMasterHex[:8]) {
		t.Errorf("the id %q shows the key", a)
	}
	if a != "3f3b91c7ca2e6906" { // RFC 5869 by hand: HMAC-SHA256 of 32 zero bytes, then of the info and 0x01
		t.Errorf("id = %s: the HKDF of the id changed, and ids no longer compare across releases", a)
	}
	if _, err := MasterID("not a key"); err == nil || !strings.Contains(err.Error(), "64 hexadecimal") {
		t.Errorf("MasterID of no key: %v, want the format", err)
	}
}

// TestHoldMasters: the keys of every source are held once each, by id, with their sources; a
// source that is not a key is an error that names it and never shows its text; none is
// ErrNoMasterKey; more than MaxMasterKeys is an error.
func TestHoldMasters(t *testing.T) {
	m := held(t, otherMasterHex, testMasterHex, otherMasterHex)
	want := []HeldKey{{ID: id(t, testMasterHex), Source: "src1"}, {ID: id(t, otherMasterHex), Source: "src0"}}
	if want[0].ID > want[1].ID {
		want[0], want[1] = want[1], want[0]
	}
	if got := m.Held(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Held = %v, want %v: a key in two sources once, under the first, by id", got, want)
	}
	if !m.Holds(id(t, testMasterHex)) || m.Holds(id(t, thirdMasterHex)) {
		t.Error("Holds is wrong")
	}

	_, err := HoldMasters([]MasterSource{{Source: "LAJSTROM_MASTER_KEY", Text: testMasterHex}, {Source: "/keys/b", Text: "zz" + testMasterHex[2:]}})
	if err == nil || !strings.Contains(err.Error(), "/keys/b") || !strings.Contains(err.Error(), "64 hexadecimal") ||
		strings.Contains(err.Error(), testMasterHex[2:10]) {
		t.Errorf("a source that is not a key: %v; want its name and the format, never its text", err)
	}
	if _, err := HoldMasters(nil); !errors.Is(err, ErrNoMasterKey) {
		t.Errorf("no source: %v, want ErrNoMasterKey", err)
	}
	var many []MasterSource
	for i := range MaxMasterKeys + 1 {
		many = append(many, MasterSource{Source: fmt.Sprint(i), Text: fmt.Sprintf("%064x", i+1)})
	}
	if _, err := HoldMasters(many[:MaxMasterKeys]); err != nil {
		t.Errorf("%d keys: %v", MaxMasterKeys, err)
	}
	if _, err := HoldMasters(many); err == nil || !strings.Contains(err.Error(), "more than 8") {
		t.Errorf("%d keys: %v, want an error", MaxMasterKeys+1, err)
	}
}

// TestSealFailsClosed: a Master seals with no key until the index names one it holds, and from
// a SealWith of a key it does not hold seals with none, the one it sealed with before included,
// with an error that matches ErrNoSealingKey, names the id, and answers 503 with Retry-After: 30;
// it opens all the while, with every key it holds.
func TestSealFailsClosed(t *testing.T) {
	m := held(t, testMasterHex, otherMasterHex)
	a, b, c := id(t, testMasterHex), id(t, otherMasterHex), id(t, thirdMasterHex)
	if _, err := m.Seal("aad", []byte("secret")); !errors.Is(err, ErrNoSealingKey) {
		t.Fatalf("a seal before the index named a key: %v, want ErrNoSealingKey", err)
	}
	if !m.SealWith(a) {
		t.Fatal("SealWith of a key held: false")
	}
	underA, err := m.Seal("aad", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if got, by, err := m.OpenKey("aad", underA); err != nil || by != a || string(got) != "secret" {
		t.Fatalf("OpenKey = %q, %s, %v; want the secret under %s", got, by, err, a)
	}
	if m.SealWith(c) {
		t.Fatal("SealWith of a key not held: true")
	}
	if id, held := m.Sealing(); id != c || held {
		t.Errorf("Sealing = %s, %t; want %s, false", id, held, c)
	}
	checkSealsNothing(t, m, c)
	if got, err := m.Open("aad", underA); err != nil || string(got) != "secret" {
		t.Errorf("Open while failing closed = %q, %v; want the secret", got, err)
	}
	if !m.SealWith(b) {
		t.Fatal("SealWith of the other key held: false")
	}
	underB, err := m.Seal("aad", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, by, _ := m.OpenKey("aad", underB); by != b {
		t.Errorf("a seal after SealWith(%s) opens with %s", b, by)
	}
}

// checkSealsNothing checks that m seals nothing, with an error that matches ErrNoSealingKey, names
// id and answers 503 with Retry-After: 30.
func checkSealsNothing(t *testing.T, m *Master, id string) {
	t.Helper()
	for name, seal := range map[string]func() error{
		"Seal":         func() error { _, err := m.Seal("aad", []byte("secret")); return err },
		"seal":         func() error { _, err := m.seal("FP", []byte("secret")); return err },
		"SealSnapshot": func() error { _, err := m.SealSnapshot([]byte("doc"), []byte("ad")); return err },
	} {
		err := seal()
		if !errors.Is(err, ErrNoSealingKey) || !strings.Contains(err.Error(), id) {
			t.Errorf("%s while the key the index names is not held: %v; want ErrNoSealingKey naming %s", name, err, id)
			continue
		}
		rec := httptest.NewRecorder()
		if status := format.WriteError(rec, err); status != 503 || rec.Header().Get("Retry-After") != "30" {
			t.Errorf("%s: answered %d, Retry-After %q; want 503, 30", name, status, rec.Header().Get("Retry-After"))
		}
	}
}

// TestOpenAnyHeld: a value sealed under any key held opens, the sealing key tried first; one
// under a key not held fails without a byte of either in the error.
func TestOpenAnyHeld(t *testing.T) {
	old, other := master(t, testMasterHex), master(t, thirdMasterHex)
	sealed, err := old.Seal("webhook:u1", []byte("hook secret"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := old.SealSnapshot([]byte("document"), []byte("ad"))
	if err != nil {
		t.Fatal(err)
	}
	m := held(t, otherMasterHex, testMasterHex)
	m.SealWith(id(t, otherMasterHex))
	if got, by, err := m.OpenKey("webhook:u1", sealed); err != nil || string(got) != "hook secret" || by != id(t, testMasterHex) {
		t.Errorf("OpenKey of a value under the other key held = %q, %s, %v", got, by, err)
	}
	if got, err := m.OpenSnapshot(snap, []byte("ad")); err != nil || string(got) != "document" {
		t.Errorf("OpenSnapshot under the other key held = %q, %v", got, err)
	}
	if _, err := other.Open("webhook:u1", sealed); err == nil || strings.Contains(err.Error(), "hook secret") {
		t.Errorf("Open under a key not held: %v", err)
	}
	if _, err := other.OpenSnapshot(snap, []byte("ad")); err == nil {
		t.Error("OpenSnapshot under a key not held succeeded")
	}
}

// TestHeldRedacted: no key of a Master of several prints, in any verb or log line; the ids and
// the sources do.
func TestHeldRedacted(t *testing.T) {
	m := held(t, testMasterHex, otherMasterHex)
	m.SealWith(id(t, otherMasterHex))
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("start", "master", m, "held", m.Held())
	for _, s := range []string{fmt.Sprint(m), fmt.Sprintf("%+v", m), fmt.Sprintf("%#v", m), logged.String()} {
		for _, k := range []string{testMasterHex, otherMasterHex} {
			if strings.Contains(s, k[:16]) || strings.Contains(s, "aead") {
				t.Errorf("a master key prints in %q", s)
			}
		}
	}
	if !strings.Contains(logged.String(), id(t, testMasterHex)) || !strings.Contains(logged.String(), "src1") {
		t.Errorf("the held keys log without their ids and sources: %s", logged.String())
	}
}
