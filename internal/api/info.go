package api

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/version"
)

// ServerInfo is what GET /api/v1/info answers of the configuration and of the plugins the
// server serves (Options.Info).
type ServerInfo struct {
	// ExternalURL is server.externalURL; empty on the dev stack when it leaves it empty.
	ExternalURL string
	// AnonymousRead is auth.anonymousRead, the server-wide switch.
	AnonymousRead bool
	// Formats are the format plugins the server serves (FormatsOf).
	Formats []FormatInfo
	// OIDC is whether the OIDC login is on (auth.oidc.issuer is set): authModes has oidc.
	OIDC bool
	// PasswordsOff is auth.localPasswords false (docs/spec/rbac.md §1, Local passwords):
	// authModes has no password, so that a UI shows no password form.
	PasswordsOff bool
	// AdminMFA is auth.admin.requireMFA, the admin mode (docs/spec/rbac.md §12): info answers
	// adminMFA.
	AdminMFA bool
}

// FormatInfo is a format plugin the server serves: its name and the artifact type of its
// manifests.
type FormatInfo struct {
	Name         string `json:"name"`
	ArtifactType string `json:"artifactType"`
}

// FormatsOf returns the name and artifact type of each of plugins, ordered by name.
func FormatsOf(plugins []format.Plugin) []FormatInfo {
	out := make([]FormatInfo, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, FormatInfo{Name: p.Name(), ArtifactType: p.ArtifactType()})
	}
	slices.SortFunc(out, func(a, b FormatInfo) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Info is the answer of GET /api/v1/info (docs/api.md, Server information): the build, the
// facts of ServerInfo, how a person signs in, and the caller.
type Info struct {
	Version       string       `json:"version"`
	Commit        string       `json:"commit"`
	Date          string       `json:"date"`
	ExternalURL   string       `json:"externalURL"`
	AnonymousRead bool         `json:"anonymousRead"`
	Formats       []FormatInfo `json:"formats"`
	AuthModes     []string     `json:"authModes"`
	Login         LoginInfo    `json:"login"`
	// AdminMFA is true while the admin mode is on (ServerInfo.AdminMFA), absent otherwise.
	AdminMFA bool   `json:"adminMFA,omitempty"`
	Caller   Caller `json:"caller"`
}

// LoginInfo is what GET /api/v1/info tells a web UI of POST /api/v1/login: that the server has
// it, whose token is DPoP-bound, and with oidc in authModes that /-/oidc/login takes return_to,
// code_challenge and dpop_jkt (docs/spec/rbac.md §10); the lifetime of its token, and how long it
// may go unused before the server refuses it (auth.login.idleTimeout, 0 off; #1277), in seconds,
// so that the UI warns the person before.
type LoginInfo struct {
	TokenLifetime int64 `json:"tokenLifetime"`
	IdleTimeout   int64 `json:"idleTimeout"`
}

// Caller is the request's principal as GET /api/v1/whoami answers it, whether the request's
// credential reaches the admin role, and, under the admin mode, whether the principal has an
// admin binding that the credential does not reach (docs/spec/rbac.md §12), so that a UI offers
// a sign-in with a second factor.
type Caller struct {
	Whoami
	Admin         bool `json:"admin"`
	AdminNeedsMFA bool `json:"adminNeedsMFA,omitempty"`
}

// authModes returns the ways a person signs in: a local user's password, over Basic and to POST
// /api/v1/login, while local passwords are on, a personal access token, and the OIDC login (#69)
// while it is on.
func (h *handler) authModes() []string {
	modes := []string{"token"}
	if !h.info.PasswordsOff {
		modes = []string{"password", "token"}
	}
	if h.info.OIDC {
		modes = append(modes, "oidc")
	}
	return modes
}

// serverInfo answers GET /api/v1/info. No authorization is asked for: anyone may read it. Unlike
// whoami, the anonymous principal gets 200, with itself as the caller.
func (h *handler) serverInfo(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	caller, err := h.caller(r)
	if err != nil {
		return 0, nil, err
	}
	b := version.Get()
	return http.StatusOK, Info{
		Version: b.Version, Commit: b.Commit, Date: b.Date,
		ExternalURL: h.info.ExternalURL, AnonymousRead: h.info.AnonymousRead,
		Formats: h.info.Formats, AuthModes: h.authModes(), Login: LoginInfo{
			TokenLifetime: int64(h.loginLifetime / time.Second), IdleTimeout: int64(h.loginIdle / time.Second),
		},
		AdminMFA: h.info.AdminMFA, Caller: caller,
	}, nil
}

// caller returns the request's principal as whoami answers it, with Admin set when the
// request's credential reaches the admin role (manage on auth.System, as creating a repo
// asks), and AdminNeedsMFA when the admin mode leaves out an admin binding of the principal
// for this credential (auth.AdminMode). The anonymous principal is named anonymous and is never
// the admin.
func (h *handler) caller(r *http.Request) (Caller, error) {
	p := auth.PrincipalFrom(r.Context())
	if p.Kind == auth.KindAnonymous {
		return Caller{Whoami: Whoami{Principal: p.String(), Kind: string(p.Kind), Name: p.String()}}, nil
	}
	w, err := h.whoamiOf(r, p)
	if err != nil {
		return Caller{}, err
	}
	c := Caller{Whoami: w}
	switch err := h.allow(r, auth.ActionManage, auth.System, auth.System); {
	case err == nil:
		c.Admin = true
		return c, nil
	case !denied(err):
		return Caller{}, err
	}
	if am, ok := h.auth.(auth.AdminMode); ok {
		left, err := am.AdminLeftOut(r.Context(), p)
		if err != nil {
			return Caller{}, err
		}
		c.AdminNeedsMFA = left
	}
	return c, nil
}
