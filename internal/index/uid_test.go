package index

import (
	"strings"
	"testing"
	"time"
)

// TestNewUID: a uid is a version 7 UUID of the variant of RFC 9562, lowercase, whose first 48
// bits are the milliseconds of its time, so that uids sort by time; two are never the same.
func TestNewUID(t *testing.T) {
	at := time.Date(2026, 11, 9, 11, 22, 2, 234934000, time.UTC)
	u := NewUID(at)
	if !IsUID(u) || u[14] != '7' || !strings.ContainsRune("89ab", rune(u[19])) {
		t.Fatalf("NewUID = %q, not a UUID v7 of the RFC 9562 variant", u)
	}
	if got := strings.ReplaceAll(u[:13], "-", ""); got != "01a1c00b347a" {
		t.Errorf("the time of %q is %s, want the milliseconds of %s, 01a1c00b347a", u, got, at)
	}
	seen := map[string]bool{}
	prev := ""
	for i := range 1000 {
		u := NewUID(at.Add(time.Duration(i) * time.Millisecond))
		if seen[u] {
			t.Fatalf("NewUID repeated %s", u)
		}
		seen[u] = true
		if u <= prev {
			t.Fatalf("%s sorts before the uid of an earlier millisecond, %s", u, prev)
		}
		prev = u
	}
}

func TestIsUID(t *testing.T) {
	for s, want := range map[string]bool{
		"019a6c3e-7b1e-7c3d-8e2f-1234567890ab": true,
		"00000000-0000-0000-0000-000000000000": true,
		"019A6C3E-7B1E-7C3D-8E2F-1234567890AB": false, // upper case
		"019a6c3e7b1e7c3d8e2f1234567890ab":     false, // no hyphens
		"019a6c3e-7b1e-7c3d-8e2f-1234567890a":  false,
		"019a6c3e-7b1e-7c3d-8e2f-1234567890ag": false,
		"alice":                                false,
		"":                                     false,
	} {
		if got := IsUID(s); got != want {
			t.Errorf("IsUID(%q) = %v, want %v", s, got, want)
		}
	}
}
