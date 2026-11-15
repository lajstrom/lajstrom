//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile opens the file name, creating it, and takes its flock(2) lock without waiting. It
// returns the open file, which holds the lock until it is closed or the process ends, and the
// identity of the file, its device and inode; nil, "" and no error when another open file holds
// the lock.
func lockFile(name string) (*os.File, string, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|os.O_CREATE, 0o600) //nolint:gosec // the name of the database's lock file, from the configuration
	if err != nil {
		return nil, "", err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, "", nil
		}
		return nil, "", err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		_ = f.Close()
		return nil, "", errors.New("no device and inode")
	}
	return f, fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}
