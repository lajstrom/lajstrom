package main

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processScope is the boot of this host (its boot_id, random(4)) and the PID and time
// namespaces of this process (namespaces(7)): the processes of one scope name each other by the
// same PIDs and see the same start times. A kernel without time namespaces (before 5.6) has
// none to name. "" when /proc does not tell.
func processScope() string {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	pidNS, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	scope := "boot=" + strings.TrimSpace(string(boot)) + " " + pidNS
	if timeNS, err := os.Readlink("/proc/self/ns/time"); err == nil {
		scope += " " + timeNS
	}
	return scope
}

// processStart is the start of the process pid, in clock ticks after the boot; "" when /proc
// does not tell.
func processStart(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	return statStart(b)
}

// statStart is field 22, starttime, of a /proc/<pid>/stat (proc_pid_stat(5)); "" when it has
// none. Field 2, the command in parentheses, may hold spaces and parentheses, so the fields
// are counted from the last ')'.
func statStart(stat []byte) string {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(string(stat[i+1:]))
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

// processAlive reports whether pid names a process: kill(2) with signal 0 is ESRCH for none,
// EPERM for a process of another user.
func processAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
