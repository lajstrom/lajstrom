package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestCredentialLimit is the table of docs/spec/rbac.md §3, steps 2 and 3, over every kind of
// credential, every action and every scope set, on the deploy token's repo and on others:
// docs/acceptance.md S-03 (a PAT with read does not push; without scopes it is its user), S-04
// (a deploy token with write pushes to its repo, gets 404 elsewhere and 403 for delete) and
// X-14 (no token ever gets delete or manage) at the unit level. Each want is the answer to
// read, write, delete and manage: ok, 403 (ErrScope) or 404 (index.ErrNotFound).
func TestCredentialLimit(t *testing.T) {
	scratch := index.Repo{ID: 10, Format: "raw", Name: "scratch"}
	byName := index.Repo{Format: "raw", Name: "scratch"} // what token.Reduce and the API know
	other := index.Repo{ID: 11, Format: "raw", Name: "other"}
	otherFormat := index.Repo{ID: 12, Format: "pypi", Name: "scratch"}
	recreated := index.Repo{ID: 99, Format: "raw", Name: "scratch"}
	deploy := func(scopes ...string) Credential {
		return Credential{Kind: CredDeploy, TokenID: 20, RepoID: 10, Repo: "raw/scratch", Scopes: scopes}
	}
	pat := func(scopes ...string) Credential {
		return Credential{Kind: CredPAT, TokenID: 1, UserID: 1, Scopes: scopes}
	}
	all := [4]string{"ok", "ok", "ok", "ok"}
	none := [4]string{"404", "404", "404", "404"}
	tests := []struct {
		name string
		c    Credential
		repo index.Repo
		want [4]string
	}{
		{"no credential", Credential{}, scratch, all},
		{"password", Credential{Kind: CredPassword}, scratch, all},
		{"pat without scopes", pat(), other, all},
		{"pat read", pat("read"), scratch, [4]string{"ok", "403", "403", "403"}},
		{"pat write", pat("write"), scratch, [4]string{"403", "ok", "403", "403"}},
		{"pat read write", pat("read", "write"), other, [4]string{"ok", "ok", "403", "403"}},
		{"pat with an unknown scope", pat("delete"), scratch, [4]string{"403", "403", "403", "403"}},
		{"deploy read", deploy("read"), scratch, [4]string{"ok", "403", "403", "403"}},
		{"deploy write", deploy("write"), scratch, [4]string{"403", "ok", "403", "403"}},
		{"deploy read write", deploy("read", "write"), scratch, [4]string{"ok", "ok", "403", "403"}},
		{"deploy read write by name", deploy("read", "write"), byName, [4]string{"ok", "ok", "403", "403"}},
		{"deploy without scopes", deploy(), scratch, [4]string{"403", "403", "403", "403"}},
		{"deploy on another repo", deploy("read", "write"), other, none},
		{"deploy on the name in another format", deploy("read", "write"), otherFormat, none},
		{"deploy on a repo of the same name made since", deploy("read", "write"), recreated, none},
		{"deploy on the system", deploy("read", "write"), index.Repo{Format: "*", Name: "*"}, none},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i, a := range []Action{ActionRead, ActionWrite, ActionDelete, ActionManage} {
				err := tt.c.Limit(a, tt.repo)
				got := "ok"
				switch {
				case errors.Is(err, ErrScope):
					got = "403"
				case errors.Is(err, index.ErrNotFound):
					got = "404"
				case err != nil:
					got = err.Error()
				}
				if got != tt.want[i] {
					t.Errorf("%s on %s/%s: %s (%v), want %s", a, tt.repo.Format, tt.repo.Name, got, err, tt.want[i])
				}
			}
		})
	}
	err := deploy("read").Limit(ActionWrite, scratch)
	if want := "auth: outside the credential's scopes: the deploy token has read"; err == nil || err.Error() != want {
		t.Errorf("message = %v, want %q", err, want)
	}
	err = pat().Limit(ActionDelete, scratch)
	if err != nil {
		t.Errorf("a PAT without scopes is limited: %v", err)
	}
	err = deploy().Limit(ActionRead, scratch)
	if err == nil || !strings.HasSuffix(err.Error(), "has no scope") {
		t.Errorf("message = %v, want it to say no scope", err)
	}
	err = deploy("read").Limit(ActionRead, other)
	if want := "index: not found: repository raw/other"; err == nil || err.Error() != want {
		t.Errorf("message = %v, want %q, the message of a repo that does not exist", err, want)
	}
}

func TestCredentialContext(t *testing.T) {
	ctx := context.Background()
	if c := CredentialFrom(ctx); c.Kind != "" || c.Limit(ActionManage, index.Repo{}) != nil {
		t.Errorf("CredentialFrom of an empty context = %+v, want the zero credential", c)
	}
	want := Credential{Kind: CredDeploy, TokenID: 20, RepoID: 10, Repo: "raw/scratch", Scopes: []string{"read"}}
	if c := CredentialFrom(WithCredential(ctx, want)); c.Kind != want.Kind || c.TokenID != 20 || c.Repo != want.Repo {
		t.Errorf("CredentialFrom = %+v, want %+v", c, want)
	}
}

// TestSecret checks the format of the secrets over 10 000 of them: the prefix, the length, the
// alphabet, no two alike; the padding of a small number and the 43 digits of the largest.
func TestSecret(t *testing.T) {
	if SecretLen != 51 {
		t.Errorf("SecretLen = %d, want 51: the prefix and 43 digits", SecretLen)
	}
	seen := map[string]bool{}
	for i := range 10000 {
		prefix := PATPrefix
		if i%2 == 1 {
			prefix = DeployPrefix
		}
		s := newSecret(prefix)
		if len(s) != SecretLen || !tokenShaped(s, prefix) || seen[s] {
			t.Fatalf("secret %d (%d characters, %.8s…) is not a new %s secret of %d characters of base 62", i, len(s), s, prefix, SecretLen)
		}
		seen[s] = true
	}
	if got := encodeSecret(PATPrefix, make([]byte, 32)); got != PATPrefix+strings.Repeat("0", 43) {
		t.Errorf("the secret of zeros = %q, want 43 zeros", got)
	}
	if got := encodeSecret(DeployPrefix, bytes.Repeat([]byte{0xff}, 32)); len(got) != SecretLen || got[len(DeployPrefix)] == '0' {
		t.Errorf("the largest secret = %q, want 43 digits without padding", got)
	}
	if got, want := encodeSecret("", []byte{61}), strings.Repeat("0", 42)+"Z"; got != want {
		t.Errorf("61 = %q, want %q: the digits are 0-9, a-z, A-Z", got, want)
	}
	for _, s := range []string{"", PATPrefix, PATPrefix + strings.Repeat("a", 42), PATPrefix + strings.Repeat("a", 44),
		PATPrefix + strings.Repeat("a", 42) + "_", DeployPrefix + strings.Repeat("a", 43)} {
		if tokenShaped(s, PATPrefix) {
			t.Errorf("%q is taken for a personal access token", s)
		}
	}
}

// TestHashSecret checks that the stored hash is the lowercase hex sha256 of the whole secret,
// prefix included.
func TestHashSecret(t *testing.T) {
	s := PATPrefix + strings.Repeat("7", 43)
	sum := sha256.Sum256([]byte(s))
	if got := HashSecret(s); got != hex.EncodeToString(sum[:]) || got != strings.ToLower(got) || len(got) != 64 {
		t.Errorf("HashSecret = %q", got)
	}
	if HashSecret(s) == HashSecret(DeployPrefix+strings.Repeat("7", 43)) {
		t.Error("the prefix is not part of the hash")
	}
}

// TestCheckPasswordTokenPrefix checks that a password cannot look like a token: the
// authenticator tells them apart by the prefix alone.
func TestCheckPasswordTokenPrefix(t *testing.T) {
	for _, p := range []string{PATPrefix + "password", DeployPrefix + "password"} {
		err := CheckPassword(p)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "must not start with ljs_pat_ or ljs_dep_") ||
			strings.Contains(err.Error(), p) {
			t.Errorf("CheckPassword(%q) = %v, want ErrInvalid naming the prefixes and not the password", p, err)
		}
		if _, err := HashPassword(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("HashPassword(%q) = %v, want ErrInvalid", p, err)
		}
	}
	if err := CheckPassword("ljs_other_password"); err != nil {
		t.Errorf("CheckPassword of another ljs_ word = %v", err)
	}
}

// TestRedactSecrets: a token secret in a text keeps its prefix only.
func TestRedactSecrets(t *testing.T) {
	pat, dep := PATPrefix+strings.Repeat("a", 43), DeployPrefix+strings.Repeat("B", 43)
	for in, want := range map[string]string{
		"/npm/internal/-/user/token/" + pat: "/npm/internal/-/user/token/ljs_pat_…",
		"a " + dep + " and " + pat + "/x":   "a ljs_dep_… and ljs_pat_…/x",
		"/npm/internal/widgets":             "/npm/internal/widgets",
		"ljs_other":                         "ljs_other",
	} {
		if got := RedactSecrets(in); got != want {
			t.Errorf("RedactSecrets(%q) = %q, want %q", in, got, want)
		}
	}
}
