package main

import (
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/terraform"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// TestArchiveTokens: a token of an archive URL downloads the reference it names, for the service
// of server.externalURL, and nothing else; /v2 grants nothing for it; serve gives the tokens to
// the terraform plugin.
func TestArchiveTokens(t *testing.T) {
	ctx := t.Context()
	k := keys.NewEphemeralTokens()
	a := newArchiveTokens(k, "https://art.example", slog.New(slog.DiscardHandler))
	r := httptest.NewRequest("GET", "https://infra.art.example/vpc/aws/1.0.0/download", nil)
	ref := "terraform/infra/vpc/aws:1.0.0"
	raw, err := a.Issue(ctx, r, auth.Principal{Kind: auth.KindUser, Name: index.NewUID(time.Now())}, ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, raw, ref string
		want           bool
	}{
		{"the reference", raw, ref, true},
		{"another version", raw, "terraform/infra/vpc/aws:1.1.0", false},
		{"no token", "not a token", ref, false},
	} {
		if ok, err := a.Allows(ctx, r, c.raw, c.ref); err != nil || ok != c.want {
			t.Errorf("%s: Allows = %v, %v, want %v", c.name, ok, err, c.want)
		}
	}
	other := newArchiveTokens(k, "https://other.example", slog.New(slog.DiscardHandler))
	if ok, err := other.Allows(ctx, r, raw, ref); err != nil || ok {
		t.Errorf("another service: Allows = %v, %v", ok, err)
	}
	g, err := token.NewVerifier(token.Options{Keys: k}).Verify(ctx, raw, "art.example")
	if err != nil {
		t.Fatal(err)
	}
	if g.Allows("terraform/infra/vpc/aws", token.ActionPull) || g.Catalog() {
		t.Errorf("the token grants something on /v2: %+v", g.Access)
	}
	plugins := withArchiveTokens(format.All(), a)
	for _, p := range plugins {
		if tp, ok := p.(terraform.Plugin); ok && tp.Tokens == nil {
			t.Error("the terraform plugin served has no archive tokens")
		}
	}
	if registered, _ := format.Lookup(terraform.Name); registered.(terraform.Plugin).Tokens != nil {
		t.Errorf("the registered terraform plugin has archive tokens")
	}
}
