package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestLockFile checks the lock of a lock file: the first take holds it and names the file by its
// device and inode; a second take, through another open file, finds it held, in this process as
// in any other; once the first file is closed, a take holds it again, with the same identity.
func TestLockFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "lajstrom.db"+lockSuffix)
	f, id, err := lockFile(name)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("this operating system does not tell Lajstrom its boot")
	}
	if err != nil || f == nil || id == "" {
		t.Fatalf("lockFile = %v, %q, %v; want the lock", f, id, err)
	}
	if g, gid, err := lockFile(name); g != nil || gid != "" || err != nil {
		t.Fatalf("a second take = %v, %q, %v; want it held", g, gid, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	g, gid, err := lockFile(name)
	if err != nil || g == nil || gid != id {
		t.Fatalf("a take after the close = %v, %q, %v; want the lock of %s", g, gid, err, id)
	}
	_ = g.Close()
}

// TestLockGone checks which rows of replicas lockGone knows to be gone: a row of this boot whose
// process held the lock file that this process holds, whatever its host and PID namespace; no
// row of another boot, of another lock file or of no lock, no row without a boot, and none while
// this process holds no lock or knows no boot.
func TestLockGone(t *testing.T) {
	self := index.Replica{Host: "this-host", PID: os.Getpid(), Process: "boot=b1 pid:[4026531836] start=7", Flock: "64769:12"}
	other := func(process, flock string) index.Replica {
		return index.Replica{Host: "another-container", PID: 1, Process: process, Flock: flock}
	}
	for _, tt := range []struct {
		name      string
		row, self index.Replica
		want      bool
	}{
		{"of this boot, in another PID namespace", other("boot=b1 pid:[4026532409] start=3", "64769:12"), self, true},
		{"of this boot, where the boot is all the scope (macOS)", other("boot=b1", "64769:12"), index.Replica{Process: "boot=b1", Flock: "64769:12"}, true},
		{"of another boot", other("boot=b0 pid:[4026532409] start=3", "64769:12"), self, false},
		{"of another lock file", other("boot=b1 pid:[4026532409] start=3", "64769:13"), self, false},
		{"of no lock", other("boot=b1 pid:[4026532409] start=3", ""), self, false},
		{"without a boot", other("", "64769:12"), self, false},
		{"while this process holds no lock", other("boot=b1 pid:[4026532409] start=3", ""), index.Replica{Process: self.Process}, false},
		{"while this process knows no boot", other("boot= pid:[4026532409]", "64769:12"), index.Replica{Process: "boot=", Flock: "64769:12"}, false},
	} {
		if got := lockGone(tt.row, tt.self); got != tt.want {
			t.Errorf("%s: lockGone(%+v, %+v) = %v, want %v", tt.name, tt.row, tt.self, got, tt.want)
		}
	}
}
