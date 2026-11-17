package auth

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// SignIn is how the person of a web UI's token authenticated at the IdP: the auth_time, the amr
// and the acr of the ID token of the OIDC login the token was minted for (docs/spec/rbac.md §10),
// each when the ID token had it. A step that needs a fresh sign-in with a second factor reads it
// (FreshSignIn).
type SignIn struct {
	// At is the ID token's auth_time: when the person last authenticated at the IdP; zero when it
	// had none, and such a sign-in is never fresh.
	At time.Time
	// AMR is the ID token's amr (RFC 8176), as the IdP wrote it; empty when it had none.
	AMR []string
	// ACR is the ID token's acr, at most MaxACR bytes; "" when it had none or a longer one.
	ACR string
}

// MaxACR bounds the acr a sign-in keeps, in bytes (docs/spec/rbac.md §10): a longer one is left
// out.
const MaxACR = 256

// FreshSignInWindow is how long a sign-in stays fresh for a step that needs one: a CI trust
// policy of the API that grants write (docs/spec/rbac.md §11). A web UI's login asks the IdP for
// a new authentication within it with prompt=login and max_age of its seconds.
const FreshSignInWindow = 10 * time.Minute

// signInLeeway is how far ahead of this server's clock an auth_time may be: the IdP's clock.
const signInLeeway = time.Minute

// factorKinds are the methods of RFC 8176 that are a factor, by kind: something the person knows,
// has, or is. The others (user, mca, rba, geo, wia, ...) are no factor of their own.
var factorKinds = map[string]string{
	"pwd": "knowledge", "pin": "knowledge", "kba": "knowledge",
	"hwk": "possession", "swk": "possession", "otp": "possession", "sms": "possession", "tel": "possession", "sc": "possession",
	"pop": "possession",
	"fpt": "inherence", "face": "inherence", "iris": "inherence", "retina": "inherence", "vbm": "inherence",
}

// SecondFactor reports whether amr shows a second factor: mfa (multiple-factor authentication,
// RFC 8176), or methods of two kinds of factor, such as pwd and otp, or hwk and pin.
func SecondFactor(amr []string) bool {
	if slices.Contains(amr, "mfa") {
		return true
	}
	kinds := map[string]bool{}
	for _, m := range amr {
		if k, ok := factorKinds[m]; ok {
			kinds[k] = true
		}
	}
	return len(kinds) >= 2
}

// FreshSignIn returns nil when cred is the token of a web UI's sign-in through OIDC (its SignIn,
// docs/spec/rbac.md §10) whose person authenticated at the IdP at most FreshSignInWindow before
// now, with a second factor (SecondFactor); else an error wrapping ErrScope that says what is
// missing. A sign-in whose ID token had no auth_time is never fresh. A password's sign-in, a
// token the person made, a deploy token, a service account's token, a client certificate, a
// Kubernetes token and a CI token never are one.
func FreshSignIn(cred Credential, now time.Time) error {
	const how = "sign in again in a web UI through OIDC (prompt=login), with a second factor"
	if cred.SignIn == nil {
		return &authError{msg: fmt.Sprintf("this needs a sign-in with a second factor within the last %s, and the request's "+
			"credential is %s: %s", window(), credentialName(cred), how), err: ErrScope}
	}
	switch age := now.Sub(cred.SignIn.At); {
	case cred.SignIn.At.IsZero():
		return &authError{msg: fmt.Sprintf("this needs a sign-in with a second factor within the last %s, and this sign-in's "+
			"ID token had no auth_time: %s", window(), how), err: ErrScope}
	case age > FreshSignInWindow:
		return &authError{msg: fmt.Sprintf("this needs a sign-in with a second factor within the last %s, and this sign-in "+
			"authenticated at the identity provider %s ago: %s", window(), age.Round(time.Second), how), err: ErrScope}
	case age < -signInLeeway:
		return &authError{msg: "the sign-in's auth_time is ahead of this server's clock: " + how, err: ErrScope}
	}
	if !SecondFactor(cred.SignIn.AMR) {
		amr := "none"
		if len(cred.SignIn.AMR) > 0 {
			amr = strings.Join(cred.SignIn.AMR, " ")
		}
		return &authError{msg: fmt.Sprintf("this needs a sign-in with a second factor, and this sign-in's amr (%s) shows none: %s",
			amr, how), err: ErrScope}
	}
	return nil
}

// window is FreshSignInWindow for a person.
func window() string { return fmt.Sprintf("%d minutes", int(FreshSignInWindow/time.Minute)) }

// credentialName names the kind of cred for a person.
func credentialName(cred Credential) string {
	switch cred.Kind {
	case CredPassword:
		return "a password"
	case CredPAT:
		return "a personal access token, or a web UI's token of a password or of a login that kept no sign-in"
	case CredDeploy:
		return "a deploy token"
	case CredOIDC:
		return "an OIDC login of the command line"
	case CredCertificate:
		return "a client certificate"
	case CredKubernetes:
		return "a Kubernetes service-account token"
	case CredCI:
		return "a CI token"
	}
	return "none"
}

// AddSignIn sets signIn in the audit details d of a change made with the token of a web UI's
// sign-in through OIDC, the credential of ctx (docs/spec/rbac.md §5): {token, authTime, amr,
// acr}, each of the last three when the sign-in kept it. It returns d, unchanged for any other
// credential.
func AddSignIn(ctx context.Context, d map[string]any) map[string]any {
	cred := CredentialFrom(ctx)
	if cred.SignIn == nil {
		return d
	}
	in := map[string]any{"token": cred.TokenID}
	if !cred.SignIn.At.IsZero() {
		in["authTime"] = cred.SignIn.At.UTC().Format(time.RFC3339)
	}
	if len(cred.SignIn.AMR) > 0 {
		in["amr"] = cred.SignIn.AMR
	}
	if cred.SignIn.ACR != "" {
		in["acr"] = cred.SignIn.ACR
	}
	d["signIn"] = in
	return d
}
