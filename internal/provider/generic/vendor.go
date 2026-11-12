package generic

import (
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
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
	vendorNexus   vendor = "nexus"
	vendorGitea   vendor = "gitea" // Forgejo too: it answers alike
	vendorJFrog   vendor = "jfrog"
	vendorZot     vendor = "zot"
	vendorDocker  vendor = "dockerhub"
	// vendorDistribution is CNCF distribution, and whatever else answers GET /v2/ as it does,
	// with Docker-Distribution-Api-Version and nothing the table knows, on a host that is not
	// a hosted service's (hostedRegistries).
	vendorDistribution vendor = "distribution"
)

// Headers of the GitLab container registry's answer to GET /v2/.
const (
	headerGitLabVersion  = "Gitlab-Container-Registry-Version"
	headerGitLabDatabase = "Gitlab-Container-Registry-Database-Enabled"
)

// What Nexus Repository, JFrog's Artifactory, Zot and distribution say about themselves on
// GET /v2/.
const (
	headerJFrogVersion = "X-Jfrog-Version" // on every answer of Artifactory and JCR
	nexusServer        = "nexus/"          // Server: Nexus/3.98.1-01 (COMMUNITY), lower-cased
	nexusRealm         = "Sonatype Nexus Repository Manager"
	zotClientHeader    = "x-zot-api-client" // in Zot's Access-Control-Allow-Headers, lower-cased
	headerAPIVersion   = "Docker-Distribution-Api-Version"
)

// ghcrMaxBlobSize is GHCR's largest layer, 10 GB.
const ghcrMaxBlobSize = 10_000_000_000

// The depths of a name below the instance prefix that the vendor table knows
// (docs/registries/quirks.md, Names and repositories):
//   - Docker Hub: a name is exactly <namespace>/<repository>, and the namespace is the prefix;
//   - GitLab behind its token service, which grants a project path and at most two components
//     more (lib/container_registry/path.rb, LEVELS_SUPPORTED = 3), with a project per Lajstrom
//     repo, <prefix><format>/<repo> (docs/registries/gitlab.md, Behind GitLab). The registry
//     alone takes any depth.
const (
	dockerHubNameDepth = 1
	gitLabNameDepth    = 4
)

// hostedRegistries are the hosts, and the parents of hosts, of hosted registries that answer
// GET /v2/ as distribution does and are not it, or not on storage that was measured: never
// vendorDistribution. ACR's geo-replicas and ECR are eventually consistent.
var hostedRegistries = []string{"docker.io", "quay.io", "azurecr.io", "pkg.dev", "gcr.io", "amazonaws.com"}

// vendorDefaults is the vendor table: what each vendor needs set that the probe cannot
// measure. A capability set here replaces what the probe would find and is not probed;
// registries[].capabilities still replaces it.
var vendorDefaults = map[vendor]config.CapabilityOverrides{
	// The probe is right for both variants (docs/registries/gitlab.md). With the metadata
	// database the registry refuses unknown artifact types, which no probe sees without a
	// push: the probe logs it. Both variants saw every write at once (lajstrom registry probe,
	// 2026-11-11, docs/registries/README.md, Consistency).
	vendorGitLab: {Consistency: config.ConsistencyStrong},
	// The probe is right. Every write was seen at once, on the filesystem, SeaweedFS and MinIO
	// for distribution (lajstrom registry probe, 2026-11-11): a blob a manifest names and the
	// registry does not serve is missing, not late.
	vendorZot:          {Consistency: config.ConsistencyStrong},
	vendorDistribution: {Consistency: config.ConsistencyStrong},
	// _catalog answers 200 with every repository of GHCR, which the probe cannot tell from a
	// catalog Lajstrom can use, and every delete through /v2 is 405 (docs/registries/ghcr.md).
	// A tag read a fraction of a second after its push is sometimes 404 (users' reports,
	// docs/registries/README.md, Consistency). A layer is at most 10 GB (GitHub's documented
	// limit), which no probe measures: the bound of a /v2 blob upload (#246).
	vendorGHCR: {Catalog: new(false), DeleteManifest: new(false), Consistency: config.ConsistencyEventual, MaxBlobSize: ghcrMaxBlobSize},
	// No name of Lajstrom's fits, three components or more below the prefix: the probe's log
	// says so (docs/registries/quirks.md, Consequences).
	vendorDocker: {MaxNameDepth: dockerHubNameDepth},
	// The probe is right: the referrers API, and no catalog for a project robot
	// (docs/registries/harbor.md).
	vendorHarbor: {},
	// The rest depends on the repository format, which the referrers probe tells: afterProbe.
	vendorNexus: {},
	// The probe is right (docs/registries/gitea.md). A chunk of unknown length after the first
	// write needs registries[].uploads.spoolUnknownLength, which is the operator's: hintSpool.
	vendorGitea: {},
	// The probe is right since it wants a catalog in the 200; HEAD lies: afterProbe.
	vendorJFrog: {},
}

// hintSpool reports whether the start hints at registries[].uploads.spoolUnknownLength for v:
// Gitea and Forgejo refuse a chunk without Content-Range into a session that holds bytes (400),
// JFrog too (416). The table never turns the flag on: it writes to disk, which is the operator's
// to allow (#425).
func hintSpool(v vendor) bool { return v == vendorGitea || v == vendorJFrog }

// ghcrMaxChunk is the largest PATCH GHCR takes, 4 MiB: one byte more is 416 "the request body is
// too large and exceeds the maximum permissible limit of 4.00MiB" (docs/registries/ghcr.md,
// Upload sessions).
const ghcrMaxChunk = 4 << 20

// vendorChunks is the vendor table's part for the chunks of OpenBlobWriter and of a client's /v2
// chunk: the largest PATCH of a vendor that takes less than streamChunk in one. GHCR is told by
// its host, so the size is known before any request (chunkBound).
var vendorChunks = map[vendor]int{vendorGHCR: ghcrMaxChunk}

// chunkBound is the largest PATCH of the registry on hostport, as Lajstrom is told it: configured,
// registries[].uploads.chunkSize, when it is set, else the vendor table's; 0 when neither says.
// The key is the way for a registry the table cannot tell by its host: GHCR behind another name,
// or a proxy that takes less in one request. A client's /v2 chunk above it is split
// (appendSplit).
func chunkBound(hostport string, configured int) int {
	if configured > 0 {
		return configured
	}
	return vendorChunks[detectVendor(identity{host: hostport})]
}

// blobChunk is the size of the chunks of OpenBlobWriter for the registry on hostport: its
// chunkBound, else streamChunk.
func blobChunk(hostport string, configured int) int {
	if b := chunkBound(hostport, configured); b > 0 {
		return b
	}
	return streamChunk
}

// quirks are what the provider does differently for a vendor beyond provider.Capabilities, as
// the vendor table and the probe decide them (afterProbe).
type quirks struct {
	// wholeBlobs: a blob GET goes without the client's Range and is answered whole (200), and
	// no blob is said to take ranges. Nexus's oci format answers every ranged GET with 500
	// (docs/registries/nexus.md, item 3); a 200 for a range is what Gitea answers anyway, and
	// what the facade, the frontends and ingest's ReadAt take.
	wholeBlobs bool
	// blobExistsByGET: StatBlob, and so Exists of a blob, asks GET of the blob's first byte, not
	// HEAD. JFrog answers HEAD 200 in any image path of the repository key once a manifest
	// anywhere names the blob, while GET there is 404 (docs/registries/jfrog-jcr.md, item 4).
	blobExistsByGET bool
}

// afterProbe is the part of the vendor table that depends on what the probe found: it changes
// probed, the capabilities before the configuration's overrides, and returns the vendor's
// quirks. referrers is whether the referrers API is used, overrides included:
//
//   - Nexus: the docker format has no referrers API, and a manifest delete by digest leaves
//     every tag that names it (docs/registries/nexus.md, item 2), so DeleteManifest is false;
//     the oci format has it, and fails every range (wholeBlobs);
//   - JFrog: blobExistsByGET.
func afterProbe(v vendor, probed *provider.Capabilities, referrers bool) quirks {
	switch v {
	case vendorNexus:
		if referrers {
			return quirks{wholeBlobs: true}
		}
		probed.DeleteManifest = false
	case vendorJFrog:
		return quirks{blobExistsByGET: true}
	case vendorUnknown, vendorGitLab, vendorGHCR, vendorHarbor, vendorGitea, vendorZot, vendorDistribution, vendorDocker:
	}
	return quirks{}
}

// vendorOverrides is the vendor table's capabilities for the registry of vendor v that id
// describes: vendorDefaults, and for GitLab behind GitLab's token service the depth the service
// grants (gitLabNameDepth).
func vendorOverrides(v vendor, id identity) config.CapabilityOverrides {
	d := vendorDefaults[v]
	if v == vendorGitLab && id.gitLabTokenService() {
		d.MaxNameDepth = gitLabNameDepth
	}
	return d
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
//   - GHCR: the host is ghcr.io;
//   - Docker Hub: the host is docker.io or below it;
//   - GitLab: GET /v2/ answers Gitlab-Container-Registry-Version;
//   - JFrog: GET /v2/ answers X-Jfrog-Version;
//   - Nexus: GET /v2/ answers Server: Nexus/<version>, or challenges with Basic and
//     realm="Sonatype Nexus Repository Manager";
//   - Harbor: GET /v2/ challenges with Bearer and service="harbor-registry";
//   - Gitea and Forgejo: GET /v2/ challenges with Bearer, service="container_registry" and
//     scope="*" (GitLab's service has the same name, without the scope);
//   - Zot: GET /v2/ answers Access-Control-Allow-Headers with X-ZOT-API-CLIENT;
//   - distribution: GET /v2/ answers Docker-Distribution-Api-Version and none of the above
//     tells, on a host that is not one of hostedRegistries.
//
// Anything else is vendorUnknown. Harbor and Gitea are told by their challenge alone, which
// the auth client meets on the first request only: one whose first request was not the
// probe's stays unknown, which changes the log line and nothing else.
func detectVendor(id identity) vendor {
	if v := vendorOfHost(id.host); v != vendorUnknown {
		return v
	}
	switch {
	case id.header.Get(headerGitLabVersion) != "":
		return vendorGitLab
	case id.header.Get(headerJFrogVersion) != "":
		return vendorJFrog
	case strings.HasPrefix(strings.ToLower(id.header.Get("Server")), nexusServer):
		return vendorNexus
	case strings.Contains(strings.ToLower(id.header.Get("Access-Control-Allow-Headers")), zotClientHeader):
		return vendorZot
	}
	scheme, realm := challengeParam(id.challenge, "realm")
	_, service := challengeParam(id.challenge, "service")
	_, scope := challengeParam(id.challenge, "scope")
	switch {
	case scheme == "basic" && realm == nexusRealm:
		return vendorNexus
	case scheme == "bearer" && service == "harbor-registry":
		return vendorHarbor
	case scheme == "bearer" && service == "container_registry" && scope == "*":
		return vendorGitea
	case id.header.Get(headerAPIVersion) != "" && !hosted(id.host):
		return vendorDistribution
	}
	return vendorUnknown
}

// vendorOfHost names the vendor that the host of the registry URL tells alone, GHCR or Docker
// Hub, else vendorUnknown.
func vendorOfHost(hostport string) vendor {
	switch {
	case isHost(hostport, "ghcr.io"):
		return vendorGHCR
	case atOrBelow(hostport, "docker.io"):
		return vendorDocker
	}
	return vendorUnknown
}

// hosted reports whether hostport is one of hostedRegistries or below one.
func hosted(hostport string) bool {
	return slices.ContainsFunc(hostedRegistries, func(h string) bool { return atOrBelow(hostport, h) })
}

// atOrBelow reports whether hostport, with or without a port, is the host name or below it.
func atOrBelow(hostport, name string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == name || strings.HasSuffix(host, "."+name)
}

// gitLabDatabase reports whether a GitLab registry keeps its metadata in its database, as
// GitLab.com does.
func (id identity) gitLabDatabase() bool {
	return strings.EqualFold(id.header.Get(headerGitLabDatabase), "true")
}

// gitLabTokenService reports whether the registry's bearer challenge has GitLab's token service
// for its realm, <GitLab>/jwt/auth, as GitLab.com's and every registry's behind a GitLab: on any
// host and port, below a relative URL root, with a trailing slash too, which Rails routes alike.
// A proxy that rewrites the realm to another path hides it: capabilities.maxNameDepth says it.
func (id identity) gitLabTokenService() bool {
	scheme, realm := challengeParam(id.challenge, "realm")
	u, err := url.Parse(realm)
	return scheme == "bearer" && err == nil && strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/jwt/auth")
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
