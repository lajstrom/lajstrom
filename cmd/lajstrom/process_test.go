package main

import (
	"os"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestProcessGone checks which rows of replicas processGone knows to be gone: a row of this host
// and scope whose PID names no process, or a process that started at another time (where the
// start is known: Linux); no row of another host, scope or binary, or without a PID.
func TestProcessGone(t *testing.T) {
	self := thisProcess()
	scope, _, _ := strings.Cut(self, " start=")
	if scope == "" {
		t.Skip("this operating system does not tell Lajstrom whether a process is gone")
	}
	const host = "this-host"
	dead := deadPID(t)
	startKnown := processStart(os.Getpid()) != ""
	for _, tt := range []struct {
		name string
		row  index.Replica
		want bool
	}{
		{"a process of this host that is gone", index.Replica{Host: host, PID: dead, Process: scope + " start=1"}, true},
		{"one whose start is not known", index.Replica{Host: host, PID: dead, Process: scope}, true},
		{"this process", index.Replica{Host: host, PID: os.Getpid(), Process: self}, false},
		{"a PID used again", index.Replica{Host: host, PID: os.Getpid(), Process: scope + " start=1"}, startKnown},
		{"another host", index.Replica{Host: "other-host", PID: dead, Process: self}, false},
		{"another boot or PID namespace", index.Replica{Host: host, PID: dead, Process: "boot=0 pid:[1] start=1"}, false},
		{"a binary before 0020", index.Replica{Host: host, PID: dead}, false},
		{"no PID", index.Replica{Host: host, Process: self}, false},
	} {
		if got := processGone(tt.row, host, self); got != tt.want {
			t.Errorf("%s: processGone(%+v) = %v, want %v", tt.name, tt.row, got, tt.want)
		}
	}
}
