package main

import (
	"os"
	"regexp"
	"testing"
)

// TestStatStart checks the start of a /proc/<pid>/stat, field 22, with a command that holds
// spaces and parentheses, and none in a line too short or without a command.
func TestStatStart(t *testing.T) {
	for _, tt := range []struct{ stat, want string }{
		{"812 (lajstrom) S 1 812 812 0 -1 4194560 2000 0 0 0 10 5 0 0 20 0 12 0 12345 1000 10\n", "12345"},
		{"7 (a) b (c)) R 1 7 7 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 678 0 0", "678"},
		{"7 (short) R 1 7 7", ""},
		{"no command", ""},
	} {
		if got := statStart([]byte(tt.stat)); got != tt.want {
			t.Errorf("statStart(%q) = %q, want %q", tt.stat, got, tt.want)
		}
	}
}

// TestThisProcessLinux checks the Process of this process on Linux: the boot, the PID
// namespace and the start, which processStart reads back.
func TestThisProcessLinux(t *testing.T) {
	want := regexp.MustCompile(`^boot=[0-9a-f-]{36} pid:\[[0-9]+\]( time:\[[0-9]+\])? start=([0-9]+)$`)
	p := thisProcess()
	m := want.FindStringSubmatch(p)
	if m == nil || m[2] != processStart(os.Getpid()) {
		t.Errorf("thisProcess() = %q, want %s with the start of processStart", p, want)
	}
}
