package oidc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// The flows of docs/spec/rbac.md §8 and their routes (docs/spec/http-routing.md §7).
const (
	// LoginTTL is how long a login request and a device request live.
	LoginTTL = 10 * time.Minute
	// PollInterval is the interval of the device flow: a poll sooner than it after the last is
	// slow_down.
	PollInterval = 5 * time.Second
	// cookieName is the cookie that binds a login to its browser.
	cookieName = "lajstrom_oidc"
	cookiePath = "/-/oidc/"
	// deviceGrant is the grant type of the device flow (RFC 8628 §3.4).
	deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
	// userCodeAlphabet are the letters of a user code (RFC 8628 §6.1): no vowels, no lookalikes.
	userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"
	userCodeLen      = 8
	// expiredRowsKept is how long an expired row stays before a new login deletes it.
	expiredRowsKept = time.Hour
)

// LoginOptions configure the handler of the OIDC routes.
type LoginOptions struct {
	// Provider is the relying party; required.
	Provider Provider
	// Index holds the login requests; Users and Tokens record the logins and mint their
	// tokens. All required.
	Index  index.Store
	Users  *auth.Users
	Tokens *auth.Tokens
	// ExternalURL is server.externalURL, the base of verification_uri.
	ExternalURL string
	// InsecureCookie leaves Secure off the cookie: server.insecureHTTP, the dev stack.
	InsecureCookie bool
	// TokenLifetime is auth.oidc.tokenLifetime.
	TokenLifetime time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Log gets the refusals at info level with their reason; nil is slog.Default().
	Log *slog.Logger
}

// handler serves /-/oidc/.
type handler struct {
	o LoginOptions
	// devices counts the device requests per client address, codes the user codes that found
	// no request.
	devices, codes *limiter
}

// NewHandler returns the handler of /-/oidc/ (docs/spec/http-routing.md §7), which the server
// mounts while auth.oidc is on. It panics when a required option is nil.
func NewHandler(o LoginOptions) http.Handler {
	if o.Provider == nil || o.Index == nil || o.Users == nil || o.Tokens == nil {
		panic("oidc: NewHandler: Provider, Index, Users and Tokens are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	h := &handler{o: o, devices: newLimiter(10, time.Minute, o.Now), codes: newLimiter(10, time.Minute, o.Now)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /-/oidc/login", h.login)
	mux.HandleFunc("GET /-/oidc/callback", h.callback)
	mux.HandleFunc("GET /-/oidc/device", h.devicePage)
	mux.HandleFunc("POST /-/oidc/device/code", h.deviceCode)
	mux.HandleFunc("POST /-/oidc/token", h.token)
	mux.HandleFunc("/-/oidc/", func(w http.ResponseWriter, _ *http.Request) {
		writePage(w, http.StatusNotFound, page{Title: "Not found", Message: "This is not a page of the login."})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

// login starts the authorization code flow: a login request with its state, nonce and PKCE
// verifier, the browser's cookie, and the redirect to the IdP. With user_code it is the login
// that approves that device request.
func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := h.o.Now()
	var device *index.OIDCLogin
	if r.URL.Query().Has("user_code") {
		var ok bool
		if device, ok = h.pendingDevice(w, r, now); !ok {
			return
		}
	}
	if _, err := h.o.Index.ExpireOIDCLogins(ctx, now.Add(-expiredRowsKept)); err != nil {
		h.o.Log.WarnContext(ctx, "oidc: expired login requests not deleted", "error", err.Error())
	}
	state, nonce, verifier, cookie := NewSecret(), NewSecret(), NewSecret(), NewSecret()
	target, err := h.o.Provider.AuthCodeURL(ctx, state, nonce, Challenge(verifier))
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	row := index.OIDCLogin{Kind: index.OIDCLoginCode, State: Hash(state), Binding: Hash(cookie), Nonce: nonce,
		Verifier: verifier, Status: index.OIDCPending, CreatedAt: now, ExpiresAt: now.Add(LoginTTL)}
	if device != nil {
		row.DeviceID = &device.ID
	}
	if err := h.o.Index.CreateOIDCLogin(ctx, &row); err != nil {
		h.fail(w, r, err)
		return
	}
	//nolint:gosec // HttpOnly and SameSite always; Secure but with server.insecureHTTP, the dev stack on loopback
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: cookie, Path: cookiePath, MaxAge: int(LoginTTL / time.Second),
		HttpOnly: true, Secure: !h.o.InsecureCookie, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, target, http.StatusFound)
}

// pendingDevice returns the device request of the user_code of r, pending and not expired; else
// it answers the device page asking again, counts the miss against the client address, and
// reports false.
func (h *handler) pendingDevice(w http.ResponseWriter, r *http.Request, now time.Time) (*index.OIDCLogin, bool) {
	ctx := r.Context()
	addr := server.ClientAddr(ctx)
	if wait := h.codes.wait(addr); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		writePage(w, http.StatusTooManyRequests, page{Title: "Too many codes", Message: "Too many codes that match no login: try again in a minute."})
		return nil, false
	}
	code := normalizeUserCode(r.URL.Query().Get("user_code"))
	d, err := h.o.Index.GetOIDCLoginByUserCode(ctx, code)
	switch {
	case errors.Is(err, index.ErrNotFound):
	case err != nil:
		h.fail(w, r, err)
		return nil, false
	case d.Kind == index.OIDCLoginDevice && d.Status == index.OIDCPending && now.Before(d.ExpiresAt):
		return d, true
	}
	h.codes.add(addr)
	h.o.Log.InfoContext(ctx, "oidc: a user code that matches no waiting device request", "address", addr)
	writeDevicePage(w, http.StatusBadRequest, "", "This code matches no login that waits: check it, or run lajstrom login again.")
	return nil, false
}

// callback is the IdP's redirect: the login request of the state, with the browser's cookie,
// redeemed once; the code exchanged and the ID token verified; the login recorded; then the new
// token shown, or the device request approved.
func (h *handler) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	row, ok := h.redeem(w, r, q.Get("state"))
	if !ok {
		return
	}
	//nolint:gosec // as the cookie of login: Secure but with server.insecureHTTP
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: cookiePath, MaxAge: -1, HttpOnly: true,
		Secure: !h.o.InsecureCookie, SameSite: http.SameSiteLaxMode})
	if e := q.Get("error"); e != "" {
		h.deny(ctx, row)
		h.refuse(w, r, fmt.Sprintf("the IdP refused the login (%s)", errorCode(e)), "The identity provider did not log you in.")
		return
	}
	id, err := h.o.Provider.Exchange(ctx, q.Get("code"), row.Verifier, row.Nonce)
	switch {
	case errors.Is(err, ErrUnavailable):
		h.unavailable(w, r, err)
		return
	case err != nil:
		h.deny(ctx, row)
		h.refuse(w, r, err.Error(), "The login did not verify. Log in again; if it fails again, tell the administrator.")
		return
	}
	u, err := h.o.Users.LoginOIDC(ctx, auth.OIDCLogin{Issuer: id.Issuer, Subject: id.Subject, Username: id.Username,
		Groups: id.Groups, Lifetime: h.o.TokenLifetime}, h.origin(r))
	switch {
	case errors.Is(err, auth.ErrLoginRefused):
		h.deny(ctx, row)
		h.refuse(w, r, err.Error(), "Lajstrom refused the login: "+err.Error()+".")
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	if row.DeviceID != nil {
		h.approve(w, r, *row.DeviceID, u)
		return
	}
	t, secret, err := h.o.Tokens.LoginToken(ctx, u, h.o.TokenLifetime, h.origin(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writePage(w, http.StatusOK, page{Title: "Logged in", User: u.Name, Token: secret, Expires: t.ExpiresAt.UTC().Format(time.RFC3339),
		Host: hostOf(h.base(r))})
}

// redeem returns the login request of state, pending, not expired, of this browser, marked
// used; else it answers the refusal and reports false.
func (h *handler) redeem(w http.ResponseWriter, r *http.Request, state string) (*index.OIDCLogin, bool) {
	ctx := r.Context()
	row, err := h.o.Index.GetOIDCLoginByState(ctx, Hash(state))
	switch {
	case state == "" || errors.Is(err, index.ErrNotFound):
		h.refuse(w, r, "an unknown state", "This login is unknown: start it again.")
		return nil, false
	case err != nil:
		h.fail(w, r, err)
		return nil, false
	}
	cookie, err := r.Cookie(cookieName)
	switch {
	case err != nil || subtle.ConstantTimeCompare([]byte(Hash(cookie.Value)), []byte(row.Binding)) != 1:
		h.refuse(w, r, "the state of another browser", "This login was started in another browser: start it again here.")
		return nil, false
	case !h.o.Now().Before(row.ExpiresAt):
		h.refuse(w, r, "an expired state", "This login took longer than 10 minutes: start it again.")
		return nil, false
	}
	row.Status = index.OIDCUsed
	switch err := h.o.Index.UpdateOIDCLogin(ctx, row, index.OIDCPending); {
	case errors.Is(err, index.ErrConflict):
		h.refuse(w, r, "a state used already", "This login was used already: start it again.")
		return nil, false
	case err != nil:
		h.fail(w, r, err)
		return nil, false
	}
	return row, true
}

// approve approves the device request id for u, once, and says to return to the terminal.
func (h *handler) approve(w http.ResponseWriter, r *http.Request, id int64, u *index.User) {
	ctx := r.Context()
	d, err := h.o.Index.GetOIDCLogin(ctx, id)
	if err == nil {
		d.Status, d.UserID = index.OIDCApproved, &u.ID
		err = h.o.Index.UpdateOIDCLogin(ctx, d, index.OIDCPending)
	}
	switch {
	case errors.Is(err, index.ErrNotFound), errors.Is(err, index.ErrConflict):
		h.refuse(w, r, "a device request that no longer waits", "The terminal's login no longer waits: run lajstrom login again.")
	case err != nil:
		h.fail(w, r, err)
	default:
		writePage(w, http.StatusOK, page{Title: "Logged in", User: u.Name, Message: "You are logged in. Return to your terminal: lajstrom login finishes there."})
	}
}

// deny marks the device request of the login row denied, when it has one that waits.
func (h *handler) deny(ctx context.Context, row *index.OIDCLogin) {
	if row.DeviceID == nil {
		return
	}
	d, err := h.o.Index.GetOIDCLogin(ctx, *row.DeviceID)
	if err == nil {
		d.Status = index.OIDCDenied
		err = h.o.Index.UpdateOIDCLogin(ctx, d, index.OIDCPending)
	}
	if err != nil && !errors.Is(err, index.ErrNotFound) && !errors.Is(err, index.ErrConflict) {
		h.o.Log.WarnContext(ctx, "oidc: a device request not denied", "error", err.Error())
	}
}

// devicePage is the page where the person types or confirms the user code.
func (h *handler) devicePage(w http.ResponseWriter, r *http.Request) {
	writeDevicePage(w, http.StatusOK, normalizeUserCode(r.URL.Query().Get("user_code")), "")
}

// deviceResponse is the answer of POST /-/oidc/device/code (RFC 8628 §3.2).
type deviceResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// deviceCode starts a device request: a device code, kept as its hash, and a user code.
func (h *handler) deviceCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	addr := server.ClientAddr(ctx)
	if wait := h.devices.wait(addr); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		writeOAuthError(w, http.StatusTooManyRequests, "slow_down", "too many device requests from this address")
		return
	}
	h.devices.add(addr)
	now := h.o.Now()
	if _, err := h.o.Index.ExpireOIDCLogins(ctx, now.Add(-expiredRowsKept)); err != nil {
		h.o.Log.WarnContext(ctx, "oidc: expired login requests not deleted", "error", err.Error())
	}
	deviceCode := NewSecret()
	row := index.OIDCLogin{Kind: index.OIDCLoginDevice, DeviceCode: Hash(deviceCode), Status: index.OIDCPending,
		CreatedAt: now, ExpiresAt: now.Add(LoginTTL)}
	var err error
	for range 3 { // a user code taken by a row that lives: another one
		row.UserCode = newUserCode()
		if err = h.o.Index.CreateOIDCLogin(ctx, &row); !errors.Is(err, index.ErrConflict) {
			break
		}
	}
	if err != nil {
		h.o.Log.ErrorContext(ctx, "oidc: a device request not made", "error", err.Error(), "requestId", server.RequestID(ctx))
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the device request could not be made")
		return
	}
	shown := row.UserCode[:4] + "-" + row.UserCode[4:]
	uri := h.base(r) + "/-/oidc/device"
	writeJSON(w, http.StatusOK, deviceResponse{DeviceCode: deviceCode, UserCode: shown, VerificationURI: uri,
		VerificationURIComplete: uri + "?user_code=" + url.QueryEscape(shown), ExpiresIn: int(LoginTTL / time.Second),
		Interval: int(PollInterval / time.Second)})
}

// tokenResponse is the answer of POST /-/oidc/token once the device is approved.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	User        string `json:"user"`
}

// token answers the polls of the device flow (RFC 8628 §3.4, §3.5): pending, slow_down,
// expired, denied, or, once approved, the token of the login, minted then, once.
func (h *handler) token(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body is not a form")
		return
	}
	if g := r.PostForm.Get("grant_type"); g != deviceGrant {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "the grant type is "+deviceGrant)
		return
	}
	now := h.o.Now()
	d, err := h.o.Index.GetOIDCLoginByDeviceCode(ctx, Hash(r.PostForm.Get("device_code")))
	switch {
	case r.PostForm.Get("device_code") == "" || errors.Is(err, index.ErrNotFound) || (err == nil && d.Kind != index.OIDCLoginDevice):
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the device code is unknown")
		return
	case err != nil:
		h.o.Log.ErrorContext(ctx, "oidc: a device request not read", "error", err.Error(), "requestId", server.RequestID(ctx))
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the device request could not be read")
		return
	case !now.Before(d.ExpiresAt):
		writeOAuthError(w, http.StatusBadRequest, "expired_token", "the device code expired: run lajstrom login again")
		return
	}
	switch d.Status {
	case index.OIDCPending:
		h.pending(w, r, d, now)
	case index.OIDCDenied:
		writeOAuthError(w, http.StatusBadRequest, "access_denied", "the login was refused")
	case index.OIDCApproved:
		h.redeemDevice(w, r, d)
	default:
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the device code was used already")
	}
}

// pending answers a poll of a device request that waits: slow_down when it comes sooner than
// PollInterval after the last poll, authorization_pending otherwise; the poll's time is kept.
func (h *handler) pending(w http.ResponseWriter, r *http.Request, d *index.OIDCLogin, now time.Time) {
	soon := d.PolledAt != nil && now.Sub(*d.PolledAt) < PollInterval
	d.PolledAt = &now
	if err := h.o.Index.UpdateOIDCLogin(r.Context(), d, index.OIDCPending); err != nil && !errors.Is(err, index.ErrConflict) {
		h.o.Log.WarnContext(r.Context(), "oidc: the time of a poll not kept", "error", err.Error())
	}
	if soon {
		writeOAuthError(w, http.StatusBadRequest, "slow_down", fmt.Sprintf("poll every %d seconds", int(PollInterval/time.Second)))
		return
	}
	writeOAuthError(w, http.StatusBadRequest, "authorization_pending", "the login has not finished yet")
}

// redeemDevice marks the approved device request redeemed, once, and mints the token of its
// login.
func (h *handler) redeemDevice(w http.ResponseWriter, r *http.Request, d *index.OIDCLogin) {
	ctx := r.Context()
	d.Status = index.OIDCRedeemed
	switch err := h.o.Index.UpdateOIDCLogin(ctx, d, index.OIDCApproved); {
	case errors.Is(err, index.ErrConflict):
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the device code was used already")
		return
	case err != nil:
		h.o.Log.ErrorContext(ctx, "oidc: a device request not redeemed", "error", err.Error(), "requestId", server.RequestID(ctx))
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the device request could not be redeemed")
		return
	}
	u, err := h.o.Index.GetUserByID(ctx, *d.UserID)
	if err == nil && (u.Disabled || u.DeletedAt != nil) {
		writeOAuthError(w, http.StatusBadRequest, "access_denied", "the user is disabled")
		return
	}
	var t *auth.Token
	var secret string
	if err == nil {
		t, secret, err = h.o.Tokens.LoginToken(ctx, u, h.o.TokenLifetime, h.origin(r))
	}
	if err != nil {
		h.o.Log.ErrorContext(ctx, "oidc: the token of a device login not made", "error", err.Error(), "requestId", server.RequestID(ctx))
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the token could not be made: run lajstrom login again")
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{AccessToken: secret, TokenType: "Bearer",
		ExpiresIn: int(t.ExpiresAt.Sub(h.o.Now()) / time.Second), User: u.Name})
}

// refuse answers 400 with a page that says what to do, msg, and logs the reason at info.
func (h *handler) refuse(w http.ResponseWriter, r *http.Request, reason, msg string) {
	h.o.Log.InfoContext(r.Context(), "oidc: login refused", "reason", reason, "address", server.ClientAddr(r.Context()),
		"requestId", server.RequestID(r.Context()))
	writePage(w, http.StatusBadRequest, page{Title: "Login refused", Message: msg, Again: true})
}

// unavailable answers 503 with Retry-After when the IdP does not answer.
func (h *handler) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	h.o.Log.WarnContext(r.Context(), "oidc: the IdP does not answer", "error", err.Error(), "requestId", server.RequestID(r.Context()))
	w.Header().Set("Retry-After", "30")
	writePage(w, http.StatusServiceUnavailable, page{Title: "Login unavailable",
		Message: "The identity provider does not answer: try again in a moment.", Again: true})
}

// fail answers 500 for a failure of the index, logged with the request id.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	h.o.Log.ErrorContext(r.Context(), "oidc: login failed", "error", err.Error(), "requestId", server.RequestID(r.Context()))
	writePage(w, http.StatusInternalServerError, page{Title: "Login failed", Message: "Lajstrom could not finish the login: try again.", Again: true})
}

// origin is the origin of the audit events of a login.
func (h *handler) origin(r *http.Request) auth.Origin {
	ip := server.ClientAddr(r.Context())
	if ip == "" {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	return auth.Origin{Source: auth.SourceOIDC, IP: ip, RequestID: server.RequestID(r.Context())}
}

// base is server.externalURL, or the request's scheme and host without it (the dev stack).
func (h *handler) base(r *http.Request) string {
	if h.o.ExternalURL != "" {
		return strings.TrimSuffix(h.o.ExternalURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// hostOf is the host[:port] of the URL base, for the examples of the token page.
func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	return u.Host
}

// newUserCode returns userCodeLen random letters of userCodeAlphabet.
func newUserCode() string {
	b := make([]byte, userCodeLen)
	limit := big.NewInt(int64(len(userCodeAlphabet)))
	for i := range b {
		n, _ := rand.Int(rand.Reader, limit) // crypto/rand.Reader does not fail
		b[i] = userCodeAlphabet[n.Int64()]
	}
	return string(b)
}

// normalizeUserCode returns a user code as typed in the form of the index: upper case, without
// hyphens and spaces; anything but the alphabet's letters makes it "", which finds nothing.
func normalizeUserCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
	if len(s) != userCodeLen || strings.Trim(s, userCodeAlphabet) != "" {
		return ""
	}
	return s
}

// writeJSON writes v as JSON with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client went away
}

// writeOAuthError writes an OAuth error (RFC 6749 §5.2).
func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}
