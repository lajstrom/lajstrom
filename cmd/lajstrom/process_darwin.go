package main

import (
	"errors"
	"syscall"
)

// processScope is the boot of this host, kern.bootsessionuuid: macOS has no PID namespaces, so
// the processes of one boot name each other by the same PIDs. "" when sysctl does not tell.
func processScope() string {
	boot, err := syscall.Sysctl("kern.bootsessionuuid")
	if err != nil || boot == "" {
		return ""
	}
	return "boot=" + boot
}

// processStart is "": the start of a process is in sysctl kern.proc.pid, which the standard
// library does not read. A PID used again by another process then counts as alive.
func processStart(int) string { return "" }

// processAlive reports whether pid names a process: kill(2) with signal 0 is ESRCH for none,
// EPERM for a process of another user.
func processAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
