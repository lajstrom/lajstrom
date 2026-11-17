package api

import (
	"cmp"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// The access report on the command line (docs/spec/rbac.md §14): its sha256, and its table for a
// spreadsheet.

// AccessDigest returns sha256:<hex> of doc, the bytes of a document as JSON wrote them.
func AccessDigest(doc []byte) string {
	sum := sha256.Sum256(doc)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// accessCSVHeader are the columns of the table of the access report.
var accessCSVHeader = []string{"principal", "kind", "name", "item", "role", "mask", "scopes", "expiresAt", "lastUsedAt", "via", "flags"}

// accessFlags are the flags in the order of a list of them.
var accessFlags = []string{FlagAdmin, FlagAllRepos, FlagAdminWithoutEffect, FlagNoExpiry, FlagUnused, FlagBeyondMaxTTL,
	FlagOwnerDisabled, FlagInactive}

// WriteCSV writes the report as a table for a spreadsheet: a header, then one line per binding,
// token and grant, in the order of the document, with the columns principal, kind (a user's
// kind, group, cert, k8s, deploy or ci), name (the entry's), item (binding, token or grant), role,
// mask (a deploy token's repo or mask), scopes and flags (joined by ;, the entry's and the
// item's), expiresAt and lastUsedAt (RFC 3339) and via. A cell that a spreadsheet would take for
// a formula, one that starts with =, +, -, @, a tab or a carriage return, gets a ' before it.
func (rep *AccessReport) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	write := func(line ...string) {
		for i := range line {
			line[i] = noFormula(line[i])
		}
		_ = cw.Write(line) // its error is the writer's, which Error keeps after Flush
	}
	write(accessCSVHeader...)
	binding := func(principal, kind, name string, b AccessBinding, flags []string) {
		write(principal, kind, name, "binding", b.Role, b.RepoMask, "", "", "", b.Via, joinFlags(flags))
	}
	token := func(principal, kind, name string, t AccessToken, flags []string) {
		write(principal, kind, name, "token", "", cmp.Or(t.Repo, t.RepoMask), strings.Join(t.Scopes, ";"), csvTime(t.ExpiresAt),
			csvTime(t.LastUsedAt), "", joinFlags(flags, t.Flags...))
	}
	for _, u := range rep.Users {
		for _, b := range u.Bindings {
			binding(u.Principal, u.Kind, u.Name, b, u.Flags)
		}
		for _, t := range u.Tokens {
			token(u.Principal, u.Kind, u.Name, t, u.Flags)
		}
	}
	for _, g := range deref(rep.Groups) {
		name := strings.TrimPrefix(g.Principal, string(auth.KindGroup)+":")
		for _, b := range g.Bindings {
			binding(g.Principal, string(auth.KindGroup), name, b, g.Flags)
		}
	}
	for _, wl := range deref(rep.Workloads) {
		kind, name, _ := strings.Cut(wl.Principal, ":")
		for _, b := range wl.Bindings {
			binding(wl.Principal, kind, name, b, wl.Flags)
		}
	}
	for _, t := range deref(rep.DeployTokens) {
		token(t.Principal, t.Kind, t.Name, t, nil)
	}
	for _, p := range deref(rep.CIPolicies) {
		for _, g := range p.Grants {
			write("", string(auth.KindCI), p.Name, "grant", g.Role, g.Mask, "", csvTime(p.ExpiresAt), "", "", joinFlags(p.Flags))
		}
	}
	cw.Flush()
	return cw.Error()
}

// joinFlags returns the flags of entry and of item, each once, in the order of the flags,
// joined by ;.
func joinFlags(entry []string, item ...string) string {
	all := slices.Concat(entry, item)
	var out []string
	for _, f := range accessFlags {
		if slices.Contains(all, f) {
			out = append(out, f)
		}
	}
	return strings.Join(out, ";")
}

// noFormula returns s with a ' before it when a spreadsheet would take it for a formula: it starts
// with =, +, -, @, a tab or a carriage return (a name of a user or a token may).
func noFormula(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// csvTime is t in RFC 3339, "" for nil.
func csvTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// deref returns *p, nil for nil.
func deref[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
}
