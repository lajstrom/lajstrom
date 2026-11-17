package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSecondFactor: with auth.oidc.mfa empty, amr shows a second factor with mfa, or with
// methods of two kinds of factor (RFC 8176); one kind, however many methods, or methods that are
// no factor, show none. With a list set, that list holds and the rule of two kinds no longer
// counts; with both set, both hold.
func TestSecondFactor(t *testing.T) {
	for amr, want := range map[string]bool{
		"mfa": true, "pwd otp": true, "hwk pin": true, "hwk fpt": true, "face swk": true, "pwd mfa": true,
		"": false, "pwd": false, "otp": false, "hwk": false, "otp sms": false, "pwd pin kba": false, "hwk user": false,
		"pwd mca rba": false, "MFA": false,
	} {
		if got := (MFA{}).SecondFactor(strings.Fields(amr), "gold"); got != want {
			t.Errorf("SecondFactor(%q) = %v, want %v", amr, got, want)
		}
	}
	hw := MFA{AMR: []string{"hwk", "swk"}}
	gold := MFA{ACR: []string{"gold", "platinum"}}
	both := MFA{AMR: []string{"hwk"}, ACR: []string{"gold"}}
	for _, tc := range []struct {
		name string
		m    MFA
		amr  string
		acr  string
		want bool
	}{
		{"one of amr", hw, "pwd swk", "", true},
		{"none of amr", hw, "pwd otp", "gold", false},
		{"mfa is not in the list", hw, "mfa", "", false},
		{"no amr", hw, "", "", false},
		{"one of acr", gold, "pwd", "platinum", true},
		{"another acr", gold, "pwd otp", "silver", false},
		{"no acr", gold, "mfa", "", false},
		{"acr compared whole", gold, "", "Gold", false},
		{"both hold", both, "hwk", "gold", true},
		{"amr only", both, "hwk", "silver", false},
		{"acr only", both, "otp", "gold", false},
	} {
		if got := tc.m.SecondFactor(strings.Fields(tc.amr), tc.acr); got != tc.want {
			t.Errorf("%s: %+v.SecondFactor(%q, %q) = %v, want %v", tc.name, tc.m, tc.amr, tc.acr, got, tc.want)
		}
	}
	var none *SignIn
	if none.SecondFactor() {
		t.Error("no sign-in shows a second factor")
	}
	if in := (&SignIn{ACR: "gold", MFA: gold}); !in.SecondFactor() {
		t.Error("a sign-in without auth_time shows the second factor of its acr")
	}
}

// TestCheckSignIn: what a step needs of a web UI's sign-in through OIDC, within 10 minutes and a
// minute ahead of the clock, with a second factor by auth.oidc.mfa. Short of it, such a sign-in is
// asked for more (a *StepUpError, max_age when the step needs a fresh one); every other credential
// gets ErrScope. Each says why.
func TestCheckSignIn(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	signed := func(ago time.Duration, amr ...string) Credential {
		return Credential{Kind: CredPAT, SignIn: &SignIn{At: now.Add(-ago), AMR: amr}}
	}
	gold := Credential{Kind: CredPAT, SignIn: &SignIn{At: now.Add(-time.Hour), AMR: []string{"pwd"}, ACR: "gold",
		MFA: MFA{ACR: []string{"gold"}}}}
	noTime := Credential{Kind: CredPAT, SignIn: &SignIn{AMR: []string{"mfa"}}}
	both, fresh, factor := Need{Fresh: true, SecondFactor: true}, Need{Fresh: true}, Need{SecondFactor: true}
	for name, tc := range map[string]struct {
		cred    Credential
		need    Need
		why     string // "" passes
		missing string // "" is ErrScope, a 403; else a step-up of that error_description
	}{
		"fresh with mfa":          {signed(time.Minute, "mfa"), both, "", ""},
		"at the window's end":     {signed(10*time.Minute, "pwd", "otp"), both, "", ""},
		"a minute ahead":          {signed(-time.Minute, "mfa"), both, "", ""},
		"stale":                   {signed(10*time.Minute+time.Second, "mfa"), both, "authenticated at the identity provider 10m1s ago", "a fresh sign-in"},
		"far ahead":               {signed(-2*time.Minute, "mfa"), both, "ahead of this server's clock", "a fresh sign-in"},
		"a password only":         {signed(time.Minute, "pwd"), both, "this sign-in's amr (pwd) shows no second factor", "a second factor"},
		"no amr":                  {signed(time.Minute), both, "this sign-in's amr (none) shows no second factor", "a second factor"},
		"stale, one factor":       {signed(time.Hour, "pwd"), both, "ago, and this sign-in's amr (pwd)", "a fresh sign-in with a second factor"},
		"no auth_time":            {noTime, both, "this sign-in's ID token had no auth_time", "a fresh sign-in"},
		"no auth_time, factor":    {noTime, factor, "", ""},
		"stale, factor only":      {signed(time.Hour, "mfa"), factor, "", ""},
		"fresh only, one factor":  {signed(time.Minute, "pwd"), fresh, "", ""},
		"fresh only, stale":       {signed(time.Hour, "mfa"), fresh, "ago", "a fresh sign-in"},
		"acr of auth.oidc.mfa":    {gold, factor, "", ""},
		"acr, stale":              {gold, both, "ago", "a fresh sign-in"},
		"a password sign-in":      {Credential{Kind: CredPAT}, both, "credential is a personal access token", ""},
		"a password":              {Credential{Kind: CredPassword}, fresh, "credential is a password", ""},
		"a deploy token":          {Credential{Kind: CredDeploy}, factor, "credential is a deploy token", ""},
		"a CI token":              {Credential{Kind: CredCI}, both, "credential is a CI token", ""},
		"a client certificate":    {Credential{Kind: CredCertificate}, both, "credential is a client certificate", ""},
		"a Kubernetes token":      {Credential{Kind: CredKubernetes}, both, "credential is a Kubernetes service-account token", ""},
		"the command line's none": {Credential{}, both, "credential is none", ""},
	} {
		err := CheckSignIn(tc.cred, now, tc.need)
		var stepUp *StepUpError
		switch {
		case tc.why == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.why == "":
		case !errors.Is(err, ErrScope) || !strings.Contains(err.Error(), tc.why):
			t.Errorf("%s: %v, want ErrScope saying %q", name, err, tc.why)
		case errors.As(err, &stepUp) != (tc.missing != ""):
			t.Errorf("%s: %T, want a step-up %v", name, err, tc.missing != "")
		case stepUp != nil && (stepUp.Missing != tc.missing || (stepUp.MaxAge == FreshSignInWindow) != tc.need.Fresh):
			t.Errorf("%s: missing %q, max_age %s, want %q and max_age with a step that needs a fresh sign-in", name,
				stepUp.Missing, stepUp.MaxAge, tc.missing)
		}
	}
	if err := FreshSignIn(signed(time.Hour, "pwd"), now); !strings.Contains(err.Error(), "with a second factor within the last 10 minutes") {
		t.Errorf("FreshSignIn needs both: %v", err)
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
