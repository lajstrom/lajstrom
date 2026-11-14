package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// Accounts is what npm login and npm logout need of internal/auth: a user's password checked as
// a Basic password is, throttled as failed logins are, and a password only
// (*auth.Authenticator.AuthenticatePassword), and personal access tokens made and revoked with
// their audit rows (*auth.Tokens). serve gives the plugin one with WithAccounts; without it the
// login is 501.
type Accounts interface {
	AuthenticatePassword(ctx context.Context, username, password string) (auth.Principal, auth.Credential, error)
	CreatePAT(ctx context.Context, user, name string, scopes []string, exp auth.Expiry, o auth.Origin) (*auth.Token, string, error)
	Revoke(ctx context.Context, id int64, o auth.Origin) error
}

// WithAccounts returns the plugin with its Accounts: serve gives every plugin that has the
// method the authenticator and the tokens service.
func (p Plugin) WithAccounts(a Accounts) format.Plugin {
	p.Accounts = a
	return p
}

// maxLoginBody bounds the body of the legacy login, a CouchDB user document.
const maxLoginBody = 64 << 10

// couchUser is the prefix of the user document npm and yarn PUT to log in.
const couchUser = "org.couchdb.user:"

// login is the legacy login of npm login, npm adduser and yarn npm login (docs/spec/
// http-routing.md, npm, Login): PUT /npm/<repo>/-/user/org.couchdb.user:<name> with
// {"name", "password", …}. The password of the body is checked as a Basic password is, as the
// user of the body: the request's own credentials are not looked at (npm sends the token it had,
// revoked or not, and the server lets this route through without it), and a token in place of
// the password is refused, since a personal access token is made with a password only (#365).
// A wrong password, an unknown or a disabled user are 401 alike, a throttled one 429; nobody is
// created. A user that may not read the repo gets 404, and so does every user on a repo that
// does not exist, which withRepo serves here without a repo in the context: the answer is
// decided as the user of the body there too, so that it does not tell whether the repo exists
// (#1091). Otherwise a new personal access token of the user, not narrowed, with the default
// expiry, called npm login <repo> <time>, with its audit row (createdBy password): 201
// {"ok": true, "id", "token"}.
func (h *handlers) login(w http.ResponseWriter, r *http.Request, doc string) {
	ctx := r.Context()
	repo, found := format.RepoFrom(ctx)
	name, ok := strings.CutPrefix(doc, couchUser)
	if !ok || r.Method != http.MethodPut {
		h.fail(w, r, fmt.Errorf("%w: %s", index.ErrNotFound, r.URL.Path))
		return
	}
	if h.plugin.Accounts == nil {
		h.fail(w, r, fmt.Errorf("%w: npm login: no accounts in this server", format.ErrUnsupported))
		return
	}
	var body struct{ Name, Password string }
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLoginBody))
	if err == nil && (json.Unmarshal(b, &body) != nil || body.Name != name) {
		err = fmt.Errorf("%w: the body of a login is a user document whose name is %q", format.ErrInvalid, name)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p, cred, err := h.plugin.Accounts.AuthenticatePassword(ctx, name, body.Password)
	if err != nil {
		h.loginFailed(w, r, err)
		return
	}
	ctx = auth.WithCredential(auth.WithPrincipal(ctx, p), cred)
	if !found {
		h.fail(w, r, fmt.Errorf("%w: repository %s/%s", index.ErrNotFound, Name, r.PathValue("repoName")))
		return
	}
	if err := format.Authorize(ctx, h.deps, auth.ActionRead, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	origin := auth.Origin{Source: "npm", IP: server.ClientAddr(ctx), RequestID: server.RequestID(ctx)}
	label := "npm login " + repo.Format + "/" + repo.Name + " " + time.Now().UTC().Format(time.RFC3339)
	_, secret, err := h.plugin.Accounts.CreatePAT(ctx, name, label, nil, auth.Expiry{}, origin)
	for i := 2; errors.Is(err, index.ErrConflict) && i <= 9; i++ { // two logins in one second
		_, secret, err = h.plugin.Accounts.CreatePAT(ctx, name, label+" #"+strconv.Itoa(i), nil, auth.Expiry{}, origin)
	}
	if err != nil {
		h.fail(w, r, fmt.Errorf("npm: login of %s: %w", name, err))
		return
	}
	h.deps.Log.InfoContext(ctx, "npm: login", "repo", repo.Format+"/"+repo.Name, "user", name)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": couchUser + name, "token": secret})
}

// loginFailed answers a refused login: 401 for bad credentials, 429 with Retry-After for a
// throttled account or address, 500 for a failure to decide.
func (h *handlers) loginFailed(w http.ResponseWriter, r *http.Request, err error) {
	var throttled *auth.ThrottledError
	switch {
	case errors.As(err, &throttled):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(throttled.RetryAfter.Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed logins; try again later"})
	case errors.Is(err, auth.ErrBadCredentials):
		h.fail(w, r, fmt.Errorf("%w: the user name or the password is wrong", format.ErrUnauthenticated))
	default:
		h.fail(w, r, fmt.Errorf("npm: login: %w", err))
	}
}

// logout is npm logout: DELETE /npm/<repo>/-/user/token/<token>. When <token> is the personal
// access token the request authenticated with, it is revoked with its audit row: 200
// {"ok": true}. Any other token is 404, whoever owns it.
func (h *handlers) logout(w http.ResponseWriter, r *http.Request, secret string) {
	ctx := r.Context()
	cred := auth.CredentialFrom(ctx)
	if cred.Kind != auth.CredPAT || h.plugin.Accounts == nil {
		h.fail(w, r, fmt.Errorf("%w: the token is not the one of the request", index.ErrNotFound))
		return
	}
	t, err := h.deps.Index.GetTokenByHash(ctx, auth.HashSecret(secret))
	switch {
	case errors.Is(err, index.ErrNotFound) || (err == nil && t.ID != cred.TokenID):
		h.fail(w, r, fmt.Errorf("%w: the token is not the one of the request", index.ErrNotFound))
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	origin := auth.Origin{Source: "npm", IP: server.ClientAddr(ctx), RequestID: server.RequestID(ctx)}
	if err := h.plugin.Accounts.Revoke(ctx, t.ID, origin); err != nil {
		h.fail(w, r, fmt.Errorf("npm: logout: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
