package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// TestCIExchange: auth.ci without an issuer is neither an exchange nor an authenticator; with
// one, both, and the start log names each issuer, with the audience of server.externalURL by
// default, and each policy with its grants; a caFile that is missing stops the start, naming
// its key.
func TestCIExchange(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	c := &config.Config{Server: config.Server{ExternalURL: "https://art.example.com/"}}
	tokens := keys.NewEphemeralTokens()
	if h, a, err := ciExchange(c, nil, tokens, log); h != nil || a != nil || err != nil {
		t.Errorf("no issuer: %v, %v, %v", h, a, err)
	}
	c.Auth.CI = config.CI{TokenLifetime: config.DefaultCITokenLifetime,
		Issuers: []config.CIIssuer{{Name: "github", Platform: config.CIPlatformGitHub, Issuer: "https://token.actions.githubusercontent.com"}},
		Policies: []config.CIPolicy{{Name: "app", Issuer: "github", Claims: map[string]config.CIValues{"repository_id": {"1"}},
			Grants: []config.CIGrant{{Role: "reader", Mask: "*/*"}}}}}
	h, a, err := ciExchange(c, nil, tokens, log)
	if err != nil || h == nil || a == nil {
		t.Fatalf("an issuer: %v, %v, %v", h, a, err)
	}
	for _, want := range []string{"issuer=github platform=github", "audience=https://art.example.com\n", "policy=app issuer=github",
		"grants=\"[{Role:reader Mask:*/*}]\""} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the start log has no %q:\n%s", want, logs.String())
		}
	}
	c.Auth.CI.Issuers[0].CAFile = "/missing/ca.pem"
	if _, _, err := ciExchange(c, nil, tokens, log); err == nil || !strings.HasPrefix(err.Error(), "auth.ci.issuers[0].caFile: ") {
		t.Errorf("a missing caFile: %v", err)
	}
}
