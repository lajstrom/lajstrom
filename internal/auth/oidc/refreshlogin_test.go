package oidc

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	masterkeys "github.com/lajstrom/lajstrom/internal/keys"
)

// testRefresh is the refresh token the fake provider's exchange answers in these tests; it must
// show nowhere.
const testRefresh = "rt-LOGIN-SECRET-91c2"

// testInterval is auth.oidc.refresh.interval of these tests.
const testInterval = 15 * time.Minute

// newRefreshFixture is a loginFixture whose handler keeps the logins' refresh tokens
// (auth.oidc.refresh.interval, #1282), sealed with the master key it returns, and whose IdP
// issues testRefresh.
func newRefreshFixture(t *testing.T) (*loginFixture, *masterkeys.Master) {
	t.Helper()
	f := newLoginFixture(t)
	m, err := masterkeys.ParseMaster(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return *f.now.Load() }
	log := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.h = NewHandler(LoginOptions{Provider: f.p, Index: f.st, Users: &auth.Users{Index: f.st, Now: clock, Log: log},
		Tokens: &auth.Tokens{Index: f.st, Now: clock, Log: log}, ExternalURL: "https://lajstrom.example",
		TokenLifetime: 24 * time.Hour, ReturnOrigins: []string{"https://lajstrom.example", uiOrigin}, Refresh: testInterval,
		Sealer: m, Now: clock, Log: log})
	f.p.id.RefreshToken = testRefresh
	return f, m
}

// refreshRows claims every row of oidc_refresh, due or not, to read them.
func (f *loginFixture) refreshRows() []index.OIDCRefresh {
	f.t.Helper()
	far := f.now.Load().Add(365 * 24 * time.Hour)
	rows, err := f.st.ClaimOIDCRefresh(context.Background(), "test", far, far.Add(time.Minute), 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

// checkKept checks that the one row of oidc_refresh is the login's: joined to its token tokenID,
// of the identity of alice, its value testRefresh sealed with m and its id, due the interval
// after mintedAt less the jitter.
func (f *loginFixture) checkKept(m *masterkeys.Master, tokenID int64, mintedAt time.Time) {
	f.t.Helper()
	rows := f.refreshRows()
	id, err := f.st.GetIdentity(context.Background(), "https://idp.example", "sub-1")
	if err != nil || len(rows) != 1 {
		f.t.Fatalf("the rows of oidc_refresh: %+v; the identity %v", rows, err)
	}
	r := rows[0]
	if r.TokenID == nil || *r.TokenID != tokenID || r.LoginID != nil || r.IdentityID != id.ID || r.Failures != 0 ||
		r.NextAt.Before(mintedAt.Add(testInterval-testInterval/10)) || r.NextAt.After(mintedAt.Add(testInterval)) {
		f.t.Errorf("the row of the login: %+v; want joined to token %d, due %v less the jitter", r, tokenID, mintedAt.Add(testInterval))
	}
	if strings.Contains(string(r.Sealed), testRefresh) {
		f.t.Error("the refresh token is kept in clear")
	}
	if plain, err := m.Open(auth.RefreshAAD(r.ID), r.Sealed); err != nil || string(plain) != testRefresh {
		f.t.Errorf("the sealed value opens to %q, %v", plain, err)
	}
}

// checkLoginRefresh checks that the last user.login has refresh as want ("" for none), and that
// no audit row holds the refresh token.
func (f *loginFixture) checkLoginRefresh(want string) {
	f.t.Helper()
	evs, err := f.st.ListAudit(context.Background(), index.AuditFilter{Limit: 100})
	if err != nil || len(evs) == 0 {
		f.t.Fatalf("the audit rows: %v", err)
	}
	for _, ev := range evs {
		if strings.Contains(string(ev.Details), testRefresh) {
			f.t.Errorf("%s holds the refresh token", ev.Action)
		}
	}
	logins, _ := f.st.ListAudit(context.Background(), index.AuditFilter{Action: auth.ActionUserLogin, Limit: 1})
	got := string(logins[0].Details)
	if (want == "" && strings.Contains(got, `"refresh"`)) || (want != "" && !strings.Contains(got, `"refresh":`+want)) {
		f.t.Errorf("user.login = %s, want refresh %q", got, want)
	}
}

// TestRefreshKeptAtCallback: with the setting, the callback seals the exchange's refresh token
// with the row's id, in the login's transaction, the mint joins the row to the token, and
// user.login says refresh true; the token is in no log line, no page and no audit row.
func TestRefreshKeptAtCallback(t *testing.T) {
	f, m := newRefreshFixture(t)
	state, cookie := f.start("")
	rr := f.callback(state, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("callback: %d %s", rr.Code, rr.Body)
	}
	secret := tokenOf(t, rr.Body.String())
	f.checkLogin(secret)
	toks, _ := f.st.ListTokens(context.Background(), nil, nil)
	f.checkKept(m, toks[0].ID, *f.now.Load())
	f.checkLoginRefresh("true")
	if strings.Contains(rr.Body.String(), testRefresh) {
		t.Error("the page shows the refresh token")
	}
	f.checkNoSecrets(testRefresh, secret)
}

// TestRefreshNotIssued: a login whose IdP issued no refresh token mints its token all the same,
// keeps no row, says refresh false, and logs a warning that says to ask for offline_access. Off,
// the setting keeps nothing, and user.login has no refresh.
func TestRefreshNotIssued(t *testing.T) {
	f, _ := newRefreshFixture(t)
	f.p.id.RefreshToken = ""
	state, cookie := f.start("")
	if rr := f.callback(state, cookie); rr.Code != http.StatusOK {
		t.Fatalf("callback: %d %s", rr.Code, rr.Body)
	}
	if rows := f.refreshRows(); len(rows) != 0 {
		t.Errorf("the rows of a login without a refresh token: %+v", rows)
	}
	f.checkLoginRefresh("false")
	if !strings.Contains(f.logs.String(), "issued no refresh token") || !strings.Contains(f.logs.String(), "offline_access") {
		t.Errorf("no warning: %s", f.logs)
	}

	off := newLoginFixture(t)
	off.p.id.RefreshToken = testRefresh
	state, cookie = off.start("")
	if rr := off.callback(state, cookie); rr.Code != http.StatusOK {
		t.Fatalf("callback without the setting: %d %s", rr.Code, rr.Body)
	}
	if rows := off.refreshRows(); len(rows) != 0 {
		t.Errorf("the rows without the setting: %+v", rows)
	}
	off.checkLoginRefresh("")
	off.checkNoSecrets(testRefresh)
}

// TestRefreshSealsNothing: a replica that seals nothing answers the callback 503 with
// Retry-After: 30 before the code exchange, and the same callback finishes once it seals again.
func TestRefreshSealsNothing(t *testing.T) {
	f, m := newRefreshFixture(t)
	held, _ := m.Sealing()
	state, cookie := f.start("")
	m.SealWith("0123456789abcdef")
	rr := f.callback(state, cookie)
	f.p.mu.Lock()
	exchanged := f.p.code
	f.p.mu.Unlock()
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != "30" || exchanged != "" {
		t.Fatalf("callback on a replica that seals nothing: %d %v, the code exchanged %q", rr.Code, rr.Header(), exchanged)
	}
	if evs, _ := f.st.ListAudit(context.Background(), index.AuditFilter{}); len(evs) != 0 {
		t.Errorf("the refused callback wrote %+v", evs)
	}
	m.SealWith(held)
	if rr := f.callback(state, cookie); rr.Code != http.StatusOK {
		t.Fatalf("the callback again: %d %s", rr.Code, rr.Body)
	}
	f.checkLoginRefresh("true")
}

// TestRefreshJoinedAtEachMint: the token of a terminal's login, minted at the device's poll, and
// that of a web UI's sign-in, minted at POST /api/v1/login, are joined to their logins' rows.
func TestRefreshJoinedAtEachMint(t *testing.T) {
	f, m := newRefreshFixture(t)
	d := f.device()
	state, cookie := f.start("?user_code=" + url.QueryEscape(d.UserCode))
	if rr := f.callback(state, cookie); rr.Code != http.StatusOK {
		t.Fatalf("the device's callback: %d %s", rr.Code, rr.Body)
	}
	f.advance(2 * time.Minute)
	if code, ans := f.poll(d.DeviceCode); code != http.StatusOK {
		t.Fatalf("the poll: %d %v", code, ans)
	}
	toks, _ := f.st.ListTokens(context.Background(), nil, nil)
	f.checkKept(m, toks[0].ID, *f.now.Load())

	ui, m2 := newRefreshFixture(t)
	verifier := NewSecret()
	code := ui.handOff(uiOrigin+"/ui/sign-in", verifier)
	ui.advance(time.Minute)
	h := ui.handoffs(12 * time.Hour)
	h.Refresh = testInterval
	tok, _, err := h.Redeem(context.Background(), code, verifier, testJKT, auth.Origin{})
	if err != nil {
		t.Fatal(err)
	}
	ui.checkKept(m2, tok.ID, *ui.now.Load())
	ui.checkNoSecrets(testRefresh)
}
