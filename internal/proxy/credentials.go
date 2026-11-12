package proxy

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/lajstrom/lajstrom/internal/config"
)

// maxSecretFile bounds what is read of a credential file: a password or a token, not a document.
const maxSecretFile = 64 << 10

// LoadUpstreams returns the clients of the configured upstreams by name, with their credentials
// read from the files the configuration names (docs/configuration.md, upstreams): serve calls it
// at start, as it reads keys.masterKeyFile. A file that is missing, unreadable, empty or larger
// than a secret is an error naming the key, never the path or the content; a file that the
// group or others may read is used, with a warning. An http URL is used with a warning: what it
// serves is not authenticated.
func LoadUpstreams(cfg []config.Upstream, o UpstreamOptions) (map[string]*Upstream, error) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	out := make(map[string]*Upstream, len(cfg))
	for i, c := range cfg {
		if c.Format == config.FormatOCI {
			continue // LoadOCIUpstreams's
		}
		key := fmt.Sprintf("upstreams[%d]", i)
		auth, err := authorization(key, c.Auth, log)
		if err != nil {
			return nil, err
		}
		if c.Insecure() {
			log.Warn("an upstream is reached over plain http: what it serves is not authenticated", "upstream", c.Name)
		}
		uo := o
		uo.Authorization, uo.Log = auth, log
		u, err := NewUpstream(c, uo)
		if err != nil {
			return nil, err
		}
		out[c.Name] = u
	}
	return out, nil
}

// authorization returns the Authorization header of an upstream's credentials: Basic of the
// user and the password file or the password of its variable (resolved by config.Load), Bearer
// of the token file, "" for none.
func authorization(key string, a config.UpstreamAuth, log *slog.Logger) (string, error) {
	switch {
	case a.TokenFile != "":
		token, err := readSecret(key+".auth.tokenFile", a.TokenFile, log)
		if err != nil {
			return "", err
		}
		return "Bearer " + token, nil
	case a.PasswordFile != "":
		password, err := readSecret(key+".auth.passwordFile", a.PasswordFile, log)
		if err != nil {
			return "", err
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.Username+":"+password)), nil
	case a.PasswordEnv != "":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.Username+":"+a.Password)), nil
	}
	return "", nil
}

// readSecret reads the secret of the file at path, which key names, without the spaces and line
// breaks around it. Its errors and its warning carry the key, which the caller builds from the
// entry's index and a constant, and fixed text: no value that comes from the path or the file
// reaches them, so neither the content nor the path is ever logged; the operator finds the file
// from the key in the configuration.
func readSecret(key, path string, log *slog.Logger) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the operator names the file
	if err != nil {
		return "", fmt.Errorf("%s: the file %s", key, fileProblem(err))
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%s: the file %s", key, fileProblem(err))
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: not a regular file", key)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		log.Warn("a credential file of an upstream is open to the group or others: make it 0400 or 0600", "key", key)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFile+1))
	if err != nil {
		return "", fmt.Errorf("%s: the file %s", key, fileProblem(err))
	}
	secret := strings.TrimSpace(string(b))
	clear(b)
	switch {
	case len(b) > maxSecretFile:
		return "", fmt.Errorf("%s: the file is larger than a secret, %d bytes", key, maxSecretFile)
	case secret == "":
		return "", fmt.Errorf("%s: the file is empty", key)
	}
	return secret, nil
}

// fileProblem says what is wrong with a file in fixed words, from the class of err alone.
func fileProblem(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "does not exist"
	case errors.Is(err, fs.ErrPermission):
		return "is not readable: permission denied"
	}
	return "cannot be read"
}
