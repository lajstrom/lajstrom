package config

import (
	"net/url"
	"path/filepath"
	"strings"
)

// File returns the absolute path of the SQLite file that database.dsn names, as SQLite opens it:
// a file name, or a file: URI whose path is percent-decoded; a relative name is resolved against
// the working directory of the process, not the configuration file's (docs/configuration.md).
// The query is left out. It returns "" for postgres, for an in-memory database (:memory:,
// file::memory:, mode=memory), for SQLite's temporary database (an empty name) and for a URI
// whose authority SQLite refuses, which SQLite reports itself.
func (d Database) File() string {
	if d.Driver != "sqlite" {
		return ""
	}
	name, query, _ := strings.Cut(d.DSN, "?")
	if rest, ok := strings.CutPrefix(name, "file:"); ok {
		if q, err := url.ParseQuery(query); err == nil && q.Get("mode") == "memory" {
			return ""
		}
		rest, _, _ = strings.Cut(rest, "#")
		if after, ok := strings.CutPrefix(rest, "//"); ok {
			host, path, _ := strings.Cut(after, "/")
			if host != "" && host != "localhost" {
				return ""
			}
			rest = "/" + path
		}
		p, err := url.PathUnescape(rest)
		if err != nil {
			return ""
		}
		name = p
	}
	if name == "" || name == ":memory:" {
		return ""
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return ""
	}
	return abs
}
