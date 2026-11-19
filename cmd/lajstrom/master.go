package main

import (
	"context"
	"errors"
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

// masters is what a process read of its master keys at its start (docs/spec/index-schema.md, The
// master key and its replacement, #1288): the keys it holds, m, with the one the index names to
// seal, or none, m nil, and why, none, an error wrapping keys.ErrNoMasterKey.
type masters struct {
	m    *keys.Master
	none error
}

// loadMasters returns the master keys of k, none of which seals yet (sealingAtStart names it):
// the key of the file of keys.masterKeyFile, otherwise of the variable keys.masterKeyEnv names
// (Load has refused a configuration that sets both). No key configured, or the variable not set,
// is an error wrapping keys.ErrNoMasterKey. A file that is missing, unreadable, empty or not a key
// is an error that names the file, never its contents. A file that the group or others may read
// or write is used, with a warning on log; log nil skips the check.
func loadMasters(k config.Keys, lookupEnv func(string) (string, bool), log *slog.Logger) (*keys.Master, error) {
	var srcs []keys.MasterSource
	if k.MasterKeyFile != "" {
		src, err := readMasterFile("keys.masterKeyFile", k.MasterKeyFile, log)
		if err != nil {
			return nil, err
		}
		srcs = append(srcs, src)
	} else {
		src, err := keys.MasterFromEnv(k.MasterKeyEnv, lookupEnv)
		if err != nil {
			return nil, err
		}
		srcs = append(srcs, src)
	}
	return keys.HoldMasters(srcs)
}

// readMasterFile reads the master key of the file path, which the key of the configuration key
// names, in the encoding of the variable (64 hexadecimal characters; spaces and line breaks
// around them ignored). A file that is missing, unreadable, empty, longer than a key or not one is
// an error that names key and the file, never its contents; one that the group or others may
// read or write is used, with a warning on log (nil: no warning).
func readMasterFile(key, path string, log *slog.Logger) (keys.MasterSource, error) {
	f, err := os.Open(path) // #nosec G304 -- the operator's configuration names the file
	if err != nil {
		return keys.MasterSource{}, fmt.Errorf("%s: %w", key, err) // names the path and the cause
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return keys.MasterSource{}, fmt.Errorf("%s: %w", key, err)
	}
	if !fi.Mode().IsRegular() {
		return keys.MasterSource{}, fmt.Errorf("%s: %s is not a regular file", key, path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 && log != nil {
		log.Warn("the master key file is open to more than its owner: make it 0400 or 0600",
			key, path, "mode", fmt.Sprintf("%04o", perm))
	}
	b, err := io.ReadAll(io.LimitReader(f, maxMasterKeyFile+1))
	defer clear(b)
	switch {
	case err != nil:
		return keys.MasterSource{}, fmt.Errorf("%s: %w", key, err) // a *PathError: the path and the cause
	case strings.TrimSpace(string(b)) == "":
		_, perr := keys.MasterID("")
		return keys.MasterSource{}, fmt.Errorf("%s: the file %s is empty; %w", key, path, perr)
	case len(b) > maxMasterKeyFile:
		_, perr := keys.MasterID("")
		return keys.MasterSource{}, fmt.Errorf("%s: the file %s is longer than a master key; %w", key, path, perr)
	}
	if _, err := keys.MasterID(string(b)); err != nil {
		return keys.MasterSource{}, fmt.Errorf("%s: the file %s: %w", key, path, err)
	}
	return keys.MasterSource{Source: path, Text: string(b)}, nil
}

// holdMasters returns the master keys of the configuration (loadMasters) with the key the index
// names to seal (sealingAtStart): what serve and every command that seals hold from their start.
// None configured is masters{none: …}, and no error. log gets the mode warnings of the files.
func (a *app) holdMasters(ctx context.Context, st sealingStore, log *slog.Logger) (masters, error) {
	m, err := loadMasters(a.cfg.Keys, a.lookupEnv, log)
	if errors.Is(err, keys.ErrNoMasterKey) {
		return masters{none: err}, nil
	}
	if err != nil {
		return masters{}, err
	}
	if err := sealingAtStart(ctx, st, m, hostName()); err != nil {
		return masters{}, err
	}
	return masters{m: m}, nil
}

// masterSources names the sources of the master keys that k configures, as attributes of the log
// lines that say signing is on: keys.masterKeyFile or keys.masterKeyEnv with the path or the
// variable's name.
func masterSources(k config.Keys) []any {
	if k.MasterKeyFile != "" {
		return []any{"keys.masterKeyFile", k.MasterKeyFile}
	}
	return []any{"keys.masterKeyEnv", k.MasterKeyEnv}
}

// logMasters logs the master keys m holds, each id with its source, and the one that seals, at
// the start of serve; never a key. m nil logs nothing: signing off says why.
func logMasters(log *slog.Logger, m *keys.Master) {
	if m == nil {
		return
	}
	held := m.Held()
	attrs := make([]any, 0, len(held))
	for _, k := range held {
		attrs = append(attrs, slog.String(k.ID, k.Source))
	}
	sealing, _ := m.Sealing()
	log.Info("master keys", slog.Group("keys", attrs...), "sealing", sealing)
}
