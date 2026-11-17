package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSecondFactor: amr shows a second factor with mfa, or with methods of two kinds of factor
// (RFC 8176); one kind, however many methods, or methods that are no factor, show none.
func TestSecondFactor(t *testing.T) {
	for amr, want := range map[string]bool{
		"mfa": true, "pwd otp": true, "hwk pin": true, "hwk fpt": true, "face swk": true, "pwd mfa": true,
		"": false, "pwd": false, "otp": false, "hwk": false, "otp sms": false, "pwd pin kba": false, "hwk user": false,
		"pwd mca rba": false, "MFA": false,
	} {
		if got := SecondFactor(strings.Fields(amr)); got != want {
			t.Errorf("SecondFactor(%q) = %v, want %v", amr, got, want)
		}
	}
}

// TestFreshSignIn: only the sign-in of a web UI's token counts, within 10 minutes and a minute
// ahead of the clock, with a second factor; every refusal wraps ErrScope and says why.
func TestFreshSignIn(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	signed := func(ago time.Duration, amr ...string) Credential {
		return Credential{Kind: CredPAT, SignIn: &SignIn{At: now.Add(-ago), AMR: amr}}
	}
	for name, tc := range map[string]struct {
		cred Credential
		why  string
	}{
		"fresh with mfa":          {signed(time.Minute, "mfa"), ""},
		"at the window's end":     {signed(10*time.Minute, "pwd", "otp"), ""},
		"a minute ahead":          {signed(-time.Minute, "mfa"), ""},
		"stale":                   {signed(10*time.Minute+time.Second, "mfa"), "authenticated at the identity provider 10m1s ago"},
		"far ahead":               {signed(-2*time.Minute, "mfa"), "ahead of this server's clock"},
		"a password only":         {signed(time.Minute, "pwd"), "this sign-in's amr (pwd) shows none"},
		"no amr":                  {signed(time.Minute), "this sign-in's amr (none) shows none"},
		"no auth_time":            {Credential{Kind: CredPAT, SignIn: &SignIn{AMR: []string{"mfa"}, ACR: "gold"}}, "ID token had no auth_time"},
		"a password sign-in":      {Credential{Kind: CredPAT}, "credential is a personal access token, or a web UI's token of a password"},
		"a password":              {Credential{Kind: CredPassword}, "credential is a password"},
		"a deploy token":          {Credential{Kind: CredDeploy}, "credential is a deploy token"},
		"a CI token":              {Credential{Kind: CredCI}, "credential is a CI token"},
		"a client certificate":    {Credential{Kind: CredCertificate}, "credential is a client certificate"},
		"a Kubernetes token":      {Credential{Kind: CredKubernetes}, "credential is a Kubernetes service-account token"},
		"the command line's none": {Credential{}, "credential is none"},
	} {
		err := FreshSignIn(tc.cred, now)
		switch {
		case tc.why == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.why != "" && (!errors.Is(err, ErrScope) || !strings.Contains(err.Error(), tc.why)):
			t.Errorf("%s: %v, want ErrScope saying %q", name, err, tc.why)
		}
	}
}

// TestAddSignIn: the audit details of a change made with a web UI's sign-in name it, its token and
// each of auth_time, amr and acr it kept; any other credential adds nothing.
func TestAddSignIn(t *testing.T) {
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.FixedZone("CET", 3600))
	for name, tc := range map[string]struct {
		cred Credential
		want string
	}{
		"all three": {Credential{Kind: CredPAT, TokenID: 7, SignIn: &SignIn{At: at, AMR: []string{"pwd", "otp"}, ACR: "gold"}},
			`{"signIn":{"acr":"gold","amr":["pwd","otp"],"authTime":"2026-11-16T11:00:00Z","token":7}}`},
		"no auth_time": {Credential{Kind: CredPAT, TokenID: 8, SignIn: &SignIn{AMR: []string{"mfa"}}},
			`{"signIn":{"amr":["mfa"],"token":8}}`},
		"a password sign-in": {Credential{Kind: CredPAT, TokenID: 9}, `{}`},
		"a password":         {Credential{Kind: CredPassword}, `{}`},
	} {
		raw, err := json.Marshal(AddSignIn(WithCredential(t.Context(), tc.cred), map[string]any{}))
		if err != nil || string(raw) != tc.want {
			t.Errorf("%s: %s, %v, want %s", name, raw, err, tc.want)
		}
	}
}
