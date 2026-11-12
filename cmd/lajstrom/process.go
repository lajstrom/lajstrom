package main

import (
	"os"
	"strings"

	"github.com/lajstrom/lajstrom/internal/index"
)

// thisProcess is the Process of this process's row of replicas: its scope (processScope), then
// " start=" and its start (processStart) where the operating system tells it; "" where it tells
// no scope.
func thisProcess() string {
	scope := processScope()
	if scope == "" {
		return ""
	}
	if start := processStart(os.Getpid()); start != "" {
		return scope + " start=" + start
	}
	return scope
}

// processGone reports whether the process of a row of replicas is known to be gone, to this
// process of host whose Process is self: the row is of this host and of this scope, the boot and
// the PID namespace in which its PID names the same process as here, and that PID names no
// process, or one that started at another time than the row's (the PID was used again). Anything
// else is not known, and the row stays until it is stale: a row of another host, of another
// boot, of a container whose PID namespace is not this one, or of a binary that wrote no Process.
// A namespace's identity is used again only once the namespace is gone, every process in it with
// it, so a scope seen again is this one or of processes that are gone.
func processGone(row index.Replica, host, self string) bool {
	scope, start, _ := strings.Cut(row.Process, " start=")
	mine, _, _ := strings.Cut(self, " start=")
	switch {
	case row.Host != host, scope == "", scope != mine, row.PID <= 0:
		return false
	case !processAlive(row.PID):
		return true
	}
	now := processStart(row.PID)
	return start != "" && now != "" && now != start
}
