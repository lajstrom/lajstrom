package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestInPlaceOfPassword: a personal access token and a deploy token over a mask, made with a
// password, take a web UI's fresh sign-in through OIDC in its place, with one factor too
// (docs/spec/rbac.md §12, #1323); a sign-in that is not fresh, or has no auth_time, is asked for
// more with max_age; any other token is refused naming the rule, as before.
func TestInPlaceOfPassword(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	s := &Tokens{Now: func() time.Time { return now }}
	signed := func(at time.Time) Credential {
		return Credential{Kind: CredPAT, TokenID: 4, UserID: 1, SignIn: &SignIn{At: at, AMR: []string{"pwd"}}}
	}
	for name, may := range map[string]func(context.Context) error{"a PAT": s.MayCreatePAT, "a mask's token": s.MayCreateMaskDeploy} {
		for _, tc := range []struct {
			what  string
			cred  Credential
			why   string // "" passes
			stale bool
		}{
			{"a fresh sign-in with one factor", signed(now.Add(-9 * time.Minute)), "", false},
			{"a stale sign-in", signed(now.Add(-11 * time.Minute)), "a token in place of a password: this needs a sign-in within the " +
				"last 10 minutes, and this sign-in authenticated at the identity provider 11m0s ago", true},
			{"a sign-in without auth_time", Credential{Kind: CredPAT, SignIn: &SignIn{AMR: []string{"mfa"}}}, "had no auth_time", true},
			{"a token of a password's sign-in", Credential{Kind: CredPAT, TokenID: 5}, "fresh sign-in through OIDC, not with a token", false},
			{"a token with scopes", Credential{Kind: CredPAT, TokenID: 6, Scopes: []string{"read"}}, "not with a token", false},
			{"a deploy token", Credential{Kind: CredDeploy, TokenID: 7}, "not with a token", false},
			{"a password", Credential{Kind: CredPassword}, "", false},
		} {
			err := may(WithCredential(context.Background(), tc.cred))
			var stepUp *StepUpError
			switch {
			case tc.why == "" && err != nil:
				t.Errorf("%s, %s: %v", name, tc.what, err)
			case tc.why == "":
			case !errors.Is(err, ErrScope) || !strings.Contains(err.Error(), tc.why) || errors.As(err, &stepUp) != tc.stale:
				t.Errorf("%s, %s: %v, want ErrScope saying %q, a step-up %v", name, tc.what, err, tc.why, tc.stale)
			case tc.stale && (stepUp.MaxAge != FreshSignInWindow || stepUp.Missing != "a fresh sign-in"):
				t.Errorf("%s, %s: %+v, want max_age and a fresh sign-in missing", name, tc.what, stepUp)
			}
		}
	}
}

// TestCreateWithSignIn: a personal access token made with a fresh sign-in has createdBy
// token:<id>, the sign-in's, and signIn in its token.create row (docs/spec/rbac.md §5).
func TestCreateWithSignIn(t *testing.T) {
	f := newTokensFixture(t)
	now := f.tokens.Now()
	in := Credential{Kind: CredPAT, TokenID: 41, UserID: 1, SignIn: &SignIn{At: now.Add(-time.Minute), AMR: []string{"pwd", "otp"},
		ACR: "gold"}}
	ctx := WithCredential(WithPrincipal(context.Background(), Principal{Kind: KindUser, Name: "alice"}), in)
	tok, secret, err := f.tokens.CreatePAT(ctx, "alice", "laptop", nil, Expiry{In: time.Hour}, cli)
	if err != nil {
		t.Fatal(err)
	}
	f.secrets = append(f.secrets, secret)
	evs, err := f.st.ListAudit(ctx, index.AuditFilter{Action: ActionTokenCreate})
	if err != nil || len(evs) != 1 {
		t.Fatalf("token.create rows: %v, %v", evs, err)
	}
	var d struct {
		CreatedBy string         `json:"createdBy"`
		SignIn    map[string]any `json:"signIn"`
	}
	if err := json.Unmarshal(evs[0].Details, &d); err != nil || d.CreatedBy != "token:41" || d.SignIn["token"] != float64(41) ||
		d.SignIn["acr"] != "gold" || d.SignIn["authTime"] != now.Add(-time.Minute).Format(time.RFC3339) {
		t.Errorf("the token.create row of %d: %s", tok.ID, evs[0].Details)
	}
	if tok.SignIn != nil {
		t.Errorf("the token made has a sign-in of its own: %+v", tok.SignIn)
	}
	f.noSecrets(t)
}
