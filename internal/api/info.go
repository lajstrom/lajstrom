package api

import (
	"net/http"
	"slices"
	"strings"

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
	Caller        Caller       `json:"caller"`
}

// Caller is the request's principal as GET /api/v1/whoami answers it, and whether the
// request's credential reaches the admin role.
type Caller struct {
	Whoami
	Admin bool `json:"admin"`
}

// authModes are the ways a person signs in: a local user's password over Basic, and a personal
// access token. The OIDC login (#69) adds "oidc".
var authModes = []string{"password", "token"}

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
		Formats: h.info.Formats, AuthModes: authModes, Caller: caller,
	}, nil
}

// caller returns the request's principal as whoami answers it, with Admin set when the
// request's credential reaches the admin role (manage on auth.System, as creating a repo
// asks). The anonymous principal is named anonymous and is never the admin.
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
	case !denied(err):
		return Caller{}, err
	}
	return c, nil
}
