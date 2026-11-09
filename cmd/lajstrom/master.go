package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// maxMasterKeyFile bounds what is read of the file of keys.masterKeyFile: a master key is 64
// characters and a line break, so anything longer is not one.
const maxMasterKeyFile = 4096

// loadMaster returns the master key of serve and admin key. With keys.masterKeyFile it is read
// from that file, in the encoding of the variable (64 hexadecimal characters; spaces and line
// breaks around them ignored); otherwise from the variable keys.masterKeyEnv names
// (keys.LoadMaster), and an error wrapping keys.ErrNoMasterKey when there is none. Load has
// refused a configuration that sets both. A file that is missing, unreadable, empty or not a
// key is an error that names the file, never its contents. A file that the group or others
// may read or write is used, with a warning on log; log nil skips the check, for a second load
// in the same process.
func loadMaster(k config.Keys, lookupEnv func(string) (string, bool), log *slog.Logger) (*keys.Master, error) {
	if k.MasterKeyFile == "" {
		return keys.LoadMaster(k.MasterKeyEnv, lookupEnv)
	}
	f, err := os.Open(k.MasterKeyFile)
	if err != nil {
		return nil, fmt.Errorf("keys.masterKeyFile: %w", err) // names the path and the cause
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("keys.masterKeyFile: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("keys.masterKeyFile: %s is not a regular file", k.MasterKeyFile)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 && log != nil {
		log.Warn("the master key file is open to more than its owner: make it 0400 or 0600",
			"keys.masterKeyFile", k.MasterKeyFile, "mode", fmt.Sprintf("%04o", perm))
	}
	b, err := io.ReadAll(io.LimitReader(f, maxMasterKeyFile+1))
	defer clear(b)
	if err != nil {
		return nil, fmt.Errorf("keys.masterKeyFile: %w", err) // a *PathError: the path and the cause
	}
	if strings.TrimSpace(string(b)) == "" {
		_, perr := keys.ParseMaster("")
		return nil, fmt.Errorf("keys.masterKeyFile: the file %s is empty; %w", k.MasterKeyFile, perr)
	}
	if len(b) > maxMasterKeyFile {
		_, perr := keys.ParseMaster("")
		return nil, fmt.Errorf("keys.masterKeyFile: the file %s is longer than a master key; %w", k.MasterKeyFile, perr)
	}
	m, err := keys.ParseMaster(string(b))
	if err != nil {
		return nil, fmt.Errorf("keys.masterKeyFile: the file %s: %w", k.MasterKeyFile, err)
	}
	return m, nil
}

// masterSource names where the master key comes from, for the log lines that say signing is on.
func masterSource(k config.Keys) (key, value string) {
	if k.MasterKeyFile != "" {
		return "keys.masterKeyFile", k.MasterKeyFile
	}
	return "keys.masterKeyEnv", k.MasterKeyEnv
}
