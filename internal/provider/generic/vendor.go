package generic

import (
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/config"
)

// vendor is a registry product the provider recognises from what the registry says about
// itself, for the capabilities its probe cannot measure (docs/registries/README.md).
type vendor string

// The vendors of the table; every other registry is vendorUnknown.
const (
	vendorUnknown vendor = "unknown"
	vendorGitLab  vendor = "gitlab"
	vendorGHCR    vendor = "ghcr"
	vendorHarbor  vendor = "harbor"
)

// Headers of the GitLab container registry's answer to GET /v2/.
const (
	headerGitLabVersion  = "Gitlab-Container-Registry-Version"
	headerGitLabDatabase = "Gitlab-Container-Registry-Database-Enabled"
)

// vendorDefaults is the vendor table: what each vendor needs set that the probe cannot
// measure. A capability set here replaces what the probe would find and is not probed;
// registries[].capabilities still replaces it.
var vendorDefaults = map[vendor]config.CapabilityOverrides{
	// The probe is right for both variants (docs/registries/gitlab.md). With the metadata
	// database the registry refuses unknown artifact types, which no probe sees without a
	// push: the probe logs it.
	vendorGitLab: {},
	// _catalog answers 200 with every repository of GHCR, which the probe cannot tell from a
	// catalog Lajstrom can use, and every delete through /v2 is 405 (docs/registries/ghcr.md).
	vendorGHCR: {Catalog: new(false), DeleteManifest: new(false)},
	// The probe is right: the referrers API, and no catalog for a project robot
	// (docs/registries/harbor.md).
	vendorHarbor: {},
}

// vendorRealmHosts is the vendor table's part for token realms: for a registry known by its host
// name, the hosts besides its own that the realm of its bearer challenge is on. They get the
// credentials as registries[].tokenRealmHosts would give them (realmTransport); the
// configuration's hosts are added to these.
var vendorRealmHosts = map[string][]string{
	// GitLab.com: Bearer realm="https://gitlab.com/jwt/auth".
	"registry.gitlab.com": {"gitlab.com"},
	// Docker Hub: Bearer realm="https://auth.docker.io/token".
	"registry-1.docker.io": {"auth.docker.io"},
	"docker.io":            {"auth.docker.io"},
}

// realmHosts returns the hosts besides the registry's own that its token realm may be on: those
// of cfg and those of the vendor table for the host of base.
func realmHosts(cfg config.Registry, base *url.URL) []string {
	host := strings.ToLower(strings.TrimSuffix(base.Hostname(), "."))
	return append(slices.Clone(cfg.TokenRealmHosts), vendorRealmHosts[host]...)
}

// identity is what a registry says about itself: the host of its URL, the headers of its 200
// to GET /v2/, and the WWW-Authenticate challenge of a 401 to it that the auth client
// answered on the way.
type identity struct {
	host      string      // host:port of the registry URL
	header    http.Header // nil when the ping did not succeed
	challenge string      // "" when the ping met no 401
}

// detectVendor names the vendor of the registry that id describes:
//
//   - GitLab: GET /v2/ answers Gitlab-Container-Registry-Version;
//   - GHCR: the host is ghcr.io;
//   - Harbor: GET /v2/ challenges with Bearer and service="harbor-registry".
//
// Anything else is vendorUnknown. Harbor is told by its challenge alone, which the auth client
// meets on the first request only: a Harbor whose first request was not the probe's stays
// unknown, which changes the log line and nothing else.
func detectVendor(id identity) vendor {
	switch {
	case id.header.Get(headerGitLabVersion) != "":
		return vendorGitLab
	case isHost(id.host, "ghcr.io"):
		return vendorGHCR
	}
	if scheme, service := challengeParam(id.challenge, "service"); scheme == "bearer" && service == "harbor-registry" {
		return vendorHarbor
	}
	return vendorUnknown
}

// gitLabDatabase reports whether a GitLab registry keeps its metadata in its database, as
// GitLab.com does.
func (id identity) gitLabDatabase() bool {
	return strings.EqualFold(id.header.Get(headerGitLabDatabase), "true")
}

// isHost reports whether hostport, with or without a port, is the host name.
func isHost(hostport, name string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.EqualFold(strings.TrimSuffix(host, "."), name)
}

// challengeParam returns the auth scheme of a WWW-Authenticate challenge, lower-cased, and the
// value of its parameter name, "" when it has none. It reads one challenge, as registries send,
// with token or quoted-string values (RFC 9110 §11.2).
func challengeParam(challenge, name string) (scheme, value string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(challenge), " ")
	for {
		rest = strings.TrimLeft(rest, " \t,")
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			return strings.ToLower(scheme), ""
		}
		after = strings.TrimLeft(after, " \t")
		var v string
		if s, quoted := strings.CutPrefix(after, `"`); quoted {
			v, rest = unquote(s)
		} else {
			v, rest, _ = strings.Cut(after, ",")
		}
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.ToLower(scheme), strings.TrimSpace(v)
		}
	}
}

// unquote reads a quoted-string after its opening quote: its value, backslash escapes undone,
// and what follows the closing quote.
func unquote(s string) (value, rest string) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), ""
}
