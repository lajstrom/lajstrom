package auth

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// SignIn is how the person of a web UI's token authenticated at the IdP: the auth_time, the amr
// and the acr of the ID token of the OIDC login the token was minted for (docs/spec/rbac.md §10),
// each when the ID token had it. The steps of §11 and §12 read it (CheckSignIn).
type SignIn struct {
	// At is the ID token's auth_time: when the person last authenticated at the IdP; zero when it
	// had none, and such a sign-in is never fresh.
	At time.Time
	// AMR is the ID token's amr (RFC 8176), as the IdP wrote it; empty when it had none.
	AMR []string
	// ACR is the ID token's acr, at most MaxACR bytes; "" when it had none or a longer one.
	ACR string
	// MFA is what a second factor is on the server that authenticated the request,
	// auth.oidc.mfa: the authenticator sets it at each request, so that a change of the setting
	// reaches the sign-ins already made (SecondFactor).
	MFA MFA
}

// MaxACR bounds the acr a sign-in keeps, in bytes (docs/spec/rbac.md §10): a longer one is left
// out.
const MaxACR = 256

// SecondFactor reports whether the sign-in shows a second factor, by its amr and acr and the
// conditions of its MFA; a nil sign-in shows none.
func (s *SignIn) SecondFactor() bool {
	return s != nil && s.MFA.SecondFactor(s.AMR, s.ACR)
}

// MFA is what counts as a second factor of a sign-in: auth.oidc.mfa (docs/spec/rbac.md §10,
// #1276). The zero MFA is RFC 8176's rule.
type MFA struct {
	// AMR are the values of amr taken: one of the sign-in's must be one of them, when set.
	AMR []string
	// ACR are the values of acr taken: the sign-in's must be one of them, when set.
	ACR []string
}

// SecondFactor reports whether a sign-in with amr and acr shows a second factor under m. With
// neither list set: mfa in amr (multiple-factor authentication, RFC 8176), or methods of two
// kinds of factor, such as pwd and otp, or hwk and pin. Otherwise each list that is set holds,
// each value compared as a string, and the rule of two kinds no longer counts.
func (m MFA) SecondFactor(amr []string, acr string) bool {
	if len(m.AMR) == 0 && len(m.ACR) == 0 {
		return twoKinds(amr)
	}
	if len(m.AMR) > 0 && !slices.ContainsFunc(amr, func(v string) bool { return slices.Contains(m.AMR, v) }) {
		return false
	}
	return len(m.ACR) == 0 || acr != "" && slices.Contains(m.ACR, acr)
}

// FreshSignInWindow is how long a sign-in stays fresh for a step that needs one: a CI trust
// policy of the API that grants write (docs/spec/rbac.md §11), and a token made in place of a
// password (§12). A web UI's login asks the IdP for a new authentication within it with
// prompt=login and max_age of its seconds.
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

// twoKinds reports whether amr shows a second factor by RFC 8176 alone: mfa, or methods of two
// kinds of factor.
func twoKinds(amr []string) bool {
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

// Need is what a step needs of the request's sign-in (docs/spec/rbac.md §10): that it be fresh,
// that it show a second factor, or both.
type Need struct {
	Fresh, SecondFactor bool
}

// StepUpError is the refusal of a step to the token of a web UI's sign-in through OIDC whose
// sign-in does not meet it (docs/spec/rbac.md §10, Asking for more; RFC 9470): the API answers it
// 401 with WWW-Authenticate: DPoP error="insufficient_user_authentication", Missing as the
// error_description, and max_age of MaxAge when the step needs a fresh sign-in. The person signs
// in again, with prompt=login, and repeats the request; the token is not revoked. It wraps
// ErrScope, so that whoever does not answer it as such refuses the action.
type StepUpError struct {
	// Missing is what the sign-in lacks, fixed text for error_description.
	Missing string
	// MaxAge is FreshSignInWindow when the step needs a fresh sign-in, else zero.
	MaxAge time.Duration
	msg    string
}

func (e *StepUpError) Error() string { return e.msg }
func (e *StepUpError) Unwrap() error { return ErrScope }

// CheckSignIn returns nil when the credential cred meets what need asks of its sign-in at now:
// a fresh one, its auth_time at most FreshSignInWindow before now and at most a minute after it,
// and one with a second factor (SignIn.SecondFactor). Short of it, the token of a web UI's
// sign-in through OIDC (cred.SignIn) is asked for more, a *StepUpError; every other credential,
// which cannot sign in again so (a password, a web UI's token of a password, a personal access
// token, a deploy token, a service account's token, a client certificate, a Kubernetes token, a
// CI token, the command line's none), gets an error wrapping ErrScope saying what the step needs.
// Neither is a failed login. Every message says what is missing and how to get it.
func CheckSignIn(cred Credential, now time.Time, need Need) error {
	what, how := need.text()
	if cred.SignIn == nil {
		return &authError{msg: fmt.Sprintf("this needs %s, and the request's credential is %s: %s", what, credentialName(cred), how),
			err: ErrScope}
	}
	var why []string
	stale := false
	switch age := now.Sub(cred.SignIn.At); {
	case !need.Fresh:
	case cred.SignIn.At.IsZero():
		why, stale = append(why, "this sign-in's ID token had no auth_time"), true
	case age > FreshSignInWindow:
		why, stale = append(why, fmt.Sprintf("this sign-in authenticated at the identity provider %s ago",
			age.Round(time.Second))), true
	case age < -signInLeeway:
		why, stale = append(why, "the sign-in's auth_time is ahead of this server's clock"), true
	}
	factorless := need.SecondFactor && !cred.SignIn.SecondFactor()
	if factorless {
		why = append(why, cred.SignIn.factorText())
	}
	if len(why) == 0 {
		return nil
	}
	e := &StepUpError{Missing: Need{Fresh: stale, SecondFactor: factorless}.missing(),
		msg: fmt.Sprintf("this needs %s, and %s: %s", what, strings.Join(why, ", and "), how)}
	if need.Fresh {
		e.MaxAge = FreshSignInWindow
	}
	return e
}

// FreshSignIn is CheckSignIn of a fresh sign-in with a second factor: the step of a CI trust
// policy of the API that grants write and of its approval (docs/spec/rbac.md §11).
func FreshSignIn(cred Credential, now time.Time) error {
	return CheckSignIn(cred, now, Need{Fresh: true, SecondFactor: true})
}

// text is what n asks for and how a person gets it, for a message.
func (n Need) text() (what, how string) {
	how = "sign in again in a web UI through OIDC (prompt=login)"
	switch {
	case n.Fresh && n.SecondFactor:
		return "a sign-in with a second factor within the last " + window(), how + ", with a second factor"
	case n.SecondFactor:
		return "a sign-in with a second factor", how + ", with a second factor"
	}
	return "a sign-in within the last " + window(), how
}

// missing is what n lacks, as the fixed error_description of the challenge.
func (n Need) missing() string {
	switch {
	case n.Fresh && n.SecondFactor:
		return "a fresh sign-in with a second factor"
	case n.Fresh:
		return "a fresh sign-in"
	}
	return "a second factor"
}

// factorText says why the sign-in s shows no second factor, for a message.
func (s *SignIn) factorText() string {
	amr := "none"
	if len(s.AMR) > 0 {
		amr = strings.Join(s.AMR, " ")
	}
	if len(s.MFA.AMR) == 0 && len(s.MFA.ACR) == 0 {
		return fmt.Sprintf("this sign-in's amr (%s) shows no second factor", amr)
	}
	return fmt.Sprintf("this sign-in's amr (%s) and acr (%s) meet no condition of auth.oidc.mfa", amr, cmp.Or(s.ACR, "none"))
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
