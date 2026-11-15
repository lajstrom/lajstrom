package main

import (
	"strings"

	"github.com/lajstrom/lajstrom/internal/index"
)

// lockSuffix names the lock file of a SQLite database, next to it: <file>-replica.lock. serve
// holds its flock(2) lock while it runs (lockFile), and its row of replicas says so (Flock).
const lockSuffix = "-replica.lock"

// lockGone reports whether the process of a row of replicas is known to be gone by the lock file
// of the SQLite file (#752), to this process whose row is self: the row says that its process
// held the lock of the file that this process holds now, self.Flock, and it is of this boot. A
// flock lock is of one kernel, which drops it when the process that holds it ends, so the lock
// that this process took is held by no other process of this boot, whatever its host name or
// PID namespace: a container restarted in a new PID namespace is known so. A row of another boot
// is not known: another host's kernel, which a network file system may not tell this one of, or
// a boot before this one.
func lockGone(row, self index.Replica) bool {
	boot := bootOf(self.Process)
	return self.Flock != "" && row.Flock == self.Flock && boot != "" && bootOf(row.Process) == boot
}

// bootOf is the boot of a Process, its first field, "boot=<id>"; "" when it has none.
func bootOf(process string) string {
	first, _, _ := strings.Cut(process, " ")
	if !strings.HasPrefix(first, "boot=") || first == "boot=" {
		return ""
	}
	return first
}
