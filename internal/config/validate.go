package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// RepoNamePattern is the rule for repo names (docs/spec/manifest-profile.md §4.1), also applied
// to registry names and formats so that each is one safe URL and OCI path segment. The
// management API (internal/api) checks repo names with it too. Treat it as read-only.
var RepoNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:[-._][a-z0-9]+)*$`)

// MaxNameLen is the longest repo or registry name, in bytes (docs/spec/manifest-profile.md §4.1).
const MaxNameLen = 64

// ReservedRepoName says why a repo name that profile.ReservedRepoName names is refused, after the
// quoted name; the management API says the same.
const ReservedRepoName = "ends in blobs, .uploads or .sync, or is index.json, oci-layout, manifests, referrers or " +
	"tags, names that Zot's storage or Harbor's routes keep for themselves (ADR 0017)"

// Validate checks c and returns nil or one error per problem, joined with errors.Join. Each
// error starts with the key it is about, e.g. "repos[0].registry: …". Errors never contain
// a password.
func (c *Config) Validate() error {
	var p problems
	c.Server.validate(&p)
	c.Log.validate(&p)
	c.Database.validate(&p)
	c.Ingest.validate(&p)
	c.Reconcile.validate(&p)
	c.Auth.validate(&p)
	c.Keys.validate(&p)
	registries := map[string]int{}
	for i, r := range c.Registries {
		key := fmt.Sprintf("registries[%d]", i)
		r.validate(&p, key)
		if first, dup := registries[r.Name]; dup && r.Name != "" {
			p.add(key+".name", "%q is already used by registries[%d]", r.Name, first)
		} else {
			registries[r.Name] = i
		}
	}
	validateNesting(&p, c.Registries)
	upstreams := map[string]int{}
	for i, u := range c.Upstreams {
		key := fmt.Sprintf("upstreams[%d]", i)
		u.validate(&p, key)
		if first, dup := upstreams[u.Name]; dup && u.Name != "" {
			p.add(key+".name", "%q is already used by upstreams[%d]", u.Name, first)
		} else {
			upstreams[u.Name] = i
		}
	}
	repos := map[string]int{}
	for i, r := range c.Repos {
		key := fmt.Sprintf("repos[%d]", i)
		r.validate(&p, key)
		if _, ok := registries[r.Registry]; !ok && r.Registry != "" {
			p.add(key+".registry", "%q is not a configured registry", r.Registry)
		}
		r.validateUpstream(&p, key, c.Upstreams, upstreams)
		id := r.Format + "/" + r.Name
		if first, dup := repos[id]; dup {
			p.add(key, "%q is already declared by repos[%d]", id, first)
		} else {
			repos[id] = i
		}
	}
	for i, r := range c.Repos {
		if r.Kind == KindGroup {
			r.validateMembers(&p, fmt.Sprintf("repos[%d]", i), c.Repos, repos)
		}
	}
	return errors.Join(p...)
}

// problems collects validation errors.
type problems []error

func (p *problems) add(key, format string, args ...any) {
	*p = append(*p, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
}

func (s Server) validate(p *problems) {
	if s.Listen == "" {
		p.add("server.listen", "must not be empty")
	} else if _, _, err := net.SplitHostPort(s.Listen); err != nil {
		p.add("server.listen", "%q is not host:port", s.Listen)
	}
	s.validateExternalURL(p)
	if (s.TLS.CertFile == "") != (s.TLS.KeyFile == "") {
		p.add("server.tls", "certFile and keyFile must be set together")
	}
	if s.ShutdownTimeout <= 0 {
		p.add("server.shutdownTimeout", "must be positive")
	}
	if s.MetricsListen != "" {
		if _, _, err := net.SplitHostPort(s.MetricsListen); err != nil {
			p.add("server.metricsListen", "%q is not host:port", s.MetricsListen)
		} else if s.MetricsListen == s.Listen {
			p.add("server.metricsListen", "must not be server.listen: the metrics are never on the public listener")
		}
	}
	s.validateLimits(p)
	for i, e := range s.TrustedProxies {
		if _, err := parsePrefix(e); err != nil {
			p.add(fmt.Sprintf("server.trustedProxies[%d]", i), "%q is not an address or a CIDR prefix", e)
		}
	}
}

// validateLimits checks the bounds of server: none may be negative, and zero means the default or
// no bound, as configuration.md says of each.
func (s Server) validateLimits(p *problems) {
	for _, l := range []struct {
		key      string
		negative bool
	}{
		{"server.maxManifestSize", s.MaxManifestSize < 0},
		{"server.stallTimeout", s.StallTimeout < 0},
		{"server.maxCreatedAhead", s.MaxCreatedAhead < 0},
		{"server.maxCreatedBehind", s.MaxCreatedBehind < 0},
		{"server.maxConcurrentUploads", s.MaxConcurrentUploads < 0},
		{"server.uploadSessions.perRepo", s.UploadSessions.PerRepo < 0},
		{"server.uploadSessions.perPrincipal", s.UploadSessions.PerPrincipal < 0},
	} {
		if l.negative {
			p.add(l.key, "must not be negative")
		}
	}
}

// validateExternalURL checks server.externalURL (docs/spec/http-routing.md §1): Lajstrom owns
// the whole host and clients see it over TLS only, so https://<host>[:port] without a path,
// query or fragment; http only for a loopback host, behind server.insecureHTTP. It is required
// but for the dev stack (server.insecureHTTP): without it the token's audience and the realm
// come from the request's Host, which the client chooses (#248).
func (s Server) validateExternalURL(p *problems) {
	const key = "server.externalURL"
	if s.ExternalURL == "" {
		if !s.InsecureHTTP {
			p.add(key, "must be set to the URL clients use, https://<host>[:port]: it is the realm and the audience of the bearer tokens; "+
				"only the dev stack, with server.insecureHTTP: true, may leave it empty")
		}
		return
	}
	u, err := url.Parse(s.ExternalURL)
	switch {
	case err != nil || !isHTTP(u):
		p.add(key, "must be an absolute URL, https://<host>[:port]")
	case u.User != nil:
		p.add(key, "must not contain credentials")
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		p.add(key, "must be https://<host>[:port]: no path, query or fragment is allowed, Lajstrom owns the whole host")
	case u.Scheme == "http" && !s.InsecureHTTP:
		p.add(key, "must be https: clients see Lajstrom over TLS only; http is for the dev stack on a loopback host, with server.insecureHTTP: true")
	case u.Scheme == "http" && !isLoopback(u.Hostname()):
		p.add(key, "http is for a loopback host only (localhost, 127.0.0.1, ::1), even with server.insecureHTTP")
	}
}

// isLoopback reports whether host is localhost or a loopback address.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// TrustedPrefixes returns server.trustedProxies as prefixes, a bare address as /32 or /128,
// IPv4-mapped IPv6 as IPv4 (ADR 0011). It fails on an entry that Validate refuses.
func (s Server) TrustedPrefixes() ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(s.TrustedProxies))
	for i, e := range s.TrustedProxies {
		pfx, err := parsePrefix(e)
		if err != nil {
			return nil, fmt.Errorf("server.trustedProxies[%d]: %w", i, err)
		}
		out = append(out, pfx)
	}
	return out, nil
}

// parsePrefix parses a CIDR prefix, or a bare address as the prefix of that one host. The
// prefix is masked, and an IPv4-mapped IPv6 one is turned into IPv4.
func parsePrefix(s string) (netip.Prefix, error) {
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		a = a.Unmap().WithZone("")
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	pfx, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if a := pfx.Addr(); a.Is4In6() {
		if pfx.Bits() < 96 {
			return netip.Prefix{}, fmt.Errorf("%q: an IPv4-mapped prefix shorter than /96", s)
		}
		pfx = netip.PrefixFrom(a.Unmap(), pfx.Bits()-96)
	}
	return pfx.Masked(), nil
}

func (r Reconcile) validate(p *problems) {
	if r.Interval < 0 {
		p.add("reconcile.interval", "must not be negative")
	}
	if r.GraceAge < 0 {
		p.add("reconcile.graceAge", "must not be negative")
	}
}

func (l Log) validate(p *problems) {
	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		p.add("log.level", "%q is not debug, info, warn or error", l.Level)
	}
	if l.Format != "text" && l.Format != "json" {
		p.add("log.format", "%q is not text or json", l.Format)
	}
}

func (d Database) validate(p *problems) {
	if d.Driver != "sqlite" && d.Driver != "postgres" {
		p.add("database.driver", "%q is not sqlite or postgres", d.Driver)
	}
	if d.DSN == "" {
		p.add("database.dsn", "must not be empty")
	}
}

// validate checks the keys section. The master key itself is not read here: serve and admin
// key read the file of keys.masterKeyFile or the variable keys.masterKeyEnv names; Load checks
// that not both are set.
func (k Keys) validate(p *problems) {
	if k.Algorithm != KeysAlgorithmEd25519 && k.Algorithm != KeysAlgorithmRSA4096 {
		p.add("keys.algorithm", "%q is not %s or %s", k.Algorithm, KeysAlgorithmEd25519, KeysAlgorithmRSA4096)
	}
	if k.RetireGrace < 0 {
		p.add("keys.retireGrace", "must not be negative")
	}
	if k.TokenAlgorithm != KeysTokenES256 && k.TokenAlgorithm != KeysTokenEdDSA {
		p.add("keys.tokenAlgorithm", "%q is not %s or %s", k.TokenAlgorithm, KeysTokenES256, KeysTokenEdDSA)
	}
}

func (r Registry) validate(p *problems, key string) {
	validName(p, key+".name", r.Name)
	if r.Type != "generic" && r.Type != "ecr" {
		p.add(key+".type", "%q is not generic or ecr", r.Type)
	}
	r.validateURL(p, key)
	if !validPrefix(r.Prefix) {
		p.add(key+".prefix", `must not start with "/" and must end with "/" when set`)
	} else {
		validatePrefixGrammar(p, key+".prefix", r.Prefix)
	}
	if r.Timeout < 0 {
		p.add(key+".timeout", "must not be negative")
	}
	if r.Capabilities.MaxBlobSize < 0 {
		p.add(key+".capabilities.maxBlobSize", "must not be negative")
	}
	if r.Capabilities.MaxNameDepth < 0 {
		p.add(key+".capabilities.maxNameDepth", "must not be negative")
	}
	r.Capabilities.validateConsistency(p, key+".capabilities")
	if r.Uploads.ChunkSize < 0 || r.Uploads.ChunkSize > MaxUploadChunkSize {
		p.add(key+".uploads.chunkSize", "must be between 0 (the vendor table's) and %d bytes (256 MiB)", MaxUploadChunkSize)
	}
	a := r.Auth
	if (a.Username == "") != (a.PasswordEnv == "") {
		p.add(key+".auth", "username and passwordEnv must be set together")
	}
	if a.PasswordEnv != "" && a.Password == "" {
		p.add(key+".auth.passwordEnv", "environment variable %q is not set or empty", a.PasswordEnv)
	}
	r.validateTokenRealmHosts(p, key)
	r.validateECR(p, key)
}

// validateURL checks url: the registry's base URL, http(s)://<host>[:port] (a trailing / is
// allowed), as the generic provider takes it. The URL is never quoted in errors: it could
// carry credentials.
func (r Registry) validateURL(p *problems, key string) {
	u, err := url.Parse(r.URL)
	switch {
	case err != nil || !isHTTP(u):
		p.add(key+".url", "must be an absolute http(s) URL")
	case u.User != nil:
		p.add(key+".url", "must not contain credentials; use auth")
	case strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		p.add(key+".url", "must be the registry's base URL, http(s)://<host>[:port], with no path, query or fragment: "+
			"the API is at /v2/ of the host%s", pathHint(u.Path, key+".prefix"))
	}
}

// pathHint is what to do with the path of a registry URL, after the error: leave out /v2, which
// Lajstrom adds itself, and put the rest, a project or an owner such as the myorg of
// https://ghcr.io/myorg, in the prefix key. The path alone is quoted: the query could carry a
// token.
func pathHint(path, prefixKey string) string {
	var hint string
	path = strings.Trim(path, "/")
	if rest, ok := strings.CutPrefix(path, "v2"); ok && (rest == "" || rest[0] == '/') {
		hint, path = "; leave /v2 out, Lajstrom adds it", strings.TrimLeft(rest, "/")
	}
	if path != "" {
		hint += fmt.Sprintf("; a project or an owner goes in %s: %q", prefixKey, path+"/")
	}
	return hint
}

func (i Ingest) validate(p *problems) {
	if i.ClaimTTL < 0 {
		p.add("ingest.claimTTL", "must not be negative")
	}
	if i.VisibilityTimeout < 0 {
		p.add("ingest.visibilityTimeout", "must not be negative")
	}
}

// validateConsistency checks consistency and consistencyBound below key.
func (o CapabilityOverrides) validateConsistency(p *problems, key string) {
	switch o.Consistency {
	case "", ConsistencyStrong, ConsistencyEventual:
	default:
		p.add(key+".consistency", "%q is not %s or %s", o.Consistency, ConsistencyStrong, ConsistencyEventual)
	}
	switch {
	case o.ConsistencyBound < 0:
		p.add(key+".consistencyBound", "must not be negative")
	case o.ConsistencyBound > 0 && o.Consistency == ConsistencyStrong:
		p.add(key+".consistencyBound", "a strongly consistent registry has no lag: leave it out, or set consistency: eventual")
	}
}

// validateNesting refuses two registries on one URL, the same scheme, host and port, whose
// instance prefixes nest: one is the other or begins with it, the empty prefix included. The
// OCI names below the longer prefix are names below the shorter one too, so a repo on one
// registry would write and read a repo on the other (threat model P-1). Each pair is reported
// once, at the later registry, with both names. A registry whose URL or prefix is invalid is
// left out: Registry.validate reports it.
func validateNesting(p *problems, regs []Registry) {
	type place struct {
		origin, prefix string
		i              int
	}
	var seen []place
	for i, r := range regs {
		origin, ok := registryOrigin(r.URL)
		if !ok || !validPrefix(r.Prefix) {
			continue
		}
		for _, s := range seen {
			if s.origin == origin && (strings.HasPrefix(r.Prefix, s.prefix) || strings.HasPrefix(s.prefix, r.Prefix)) {
				p.add(fmt.Sprintf("registries[%d].prefix", i),
					"%q of registry %q nests with %q of registry %q (registries[%d]) on the same URL: a repository of one would be a repository of the other",
					r.Prefix, r.Name, s.prefix, regs[s.i].Name, s.i)
			}
		}
		seen = append(seen, place{origin: origin, prefix: r.Prefix, i: i})
	}
}

// registryOrigin is the scheme, host and port of a registry URL, lower-cased, the port written
// out when the URL leaves the default of its scheme, and the root dot of the host dropped.
func registryOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || !isHTTP(u) || u.User != nil {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return scheme + "://" + net.JoinHostPort(host, port), true
}

// validPrefix reports whether prefix is a valid registries[].prefix: empty, or not starting with
// "/" and ending with "/".
func validPrefix(prefix string) bool {
	return prefix == "" || (!strings.HasPrefix(prefix, "/") && strings.HasSuffix(prefix, "/"))
}

// reservedPrefixComponent says why a component of a registries[].prefix that
// profile.ReservedComponent names is refused, after the quoted component.
const reservedPrefixComponent = "ends in blobs, .uploads or .sync, or is index.json, oci-layout, manifests or referrers, " +
	"names that Zot's storage or Harbor's routes keep for themselves (ADR 0017)"

// validatePrefixGrammar checks the components of prefix, a registries[].prefix of validPrefix,
// against the grammar of the OCI names Lajstrom writes (ADR 0017): every name on the registry
// starts with the prefix, so a component that breaks a rule breaks every name. Each component
// keeps to the 1.0 grammar, which Gitea and Forgejo check (RepoNamePattern), and is no
// profile.ReservedComponent; and no component tags comes before one that starts with list
// (profile.PackagePath). The component after the prefix is a format, which never starts with
// list.
func validatePrefixGrammar(p *problems, key, prefix string) {
	if prefix == "" {
		return
	}
	parts := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
	for _, c := range parts {
		switch {
		case !RepoNamePattern.MatchString(c):
			p.add(key, "%q: component %q must be lowercase letters and digits separated by single '.', '_' or '-' "+
				"(ADR 0017, which Gitea and Forgejo check)", prefix, c)
		case profile.ReservedComponent(c):
			p.add(key, "%q: component %q %s", prefix, c, reservedPrefixComponent)
		}
	}
	if profile.PackagePath(parts...) != strings.Join(parts, "/") {
		p.add(key, "%q: a component tags before one that starts with list is the tag list to Harbor's routes (ADR 0017)", prefix)
	}
}

// validateECR checks the keys of type ecr: its own, and no auth.
func (r Registry) validateECR(p *problems, key string) {
	switch {
	case r.Type == "ecr":
		if r.Auth.Username != "" || r.Auth.PasswordEnv != "" {
			p.add(key+".auth", "must be empty for type ecr: the credentials come from the AWS SDK's default chain")
		}
		r.ECR.validate(p, key+".ecr")
	case !r.ECR.IsZero():
		p.add(key+".ecr", "is only for type ecr")
	}
}

// The values of registries[].ecr.imageTagMutability and .encryption; "" is the first of each.
var (
	ecrMutabilities = []string{"IMMUTABLE", "IMMUTABLE_WITH_EXCLUSION", "MUTABLE", "MUTABLE_WITH_EXCLUSION"}
	ecrEncryptions  = []string{"AES256", "KMS", "KMS_DSSE"}
)

func (e ECR) validate(p *problems, key string) {
	if e.Endpoint != "" {
		if u, err := url.Parse(e.Endpoint); err != nil || !isHTTP(u) || u.User != nil {
			p.add(key+".endpoint", "must be an absolute http(s) URL without credentials")
		}
	}
	e.validateMutability(p, key)
	if e.Encryption != "" && !slices.Contains(ecrEncryptions, e.Encryption) {
		p.add(key+".encryption", "%q is not one of %s", e.Encryption, strings.Join(ecrEncryptions, ", "))
	}
	if e.KMSKey != "" && !strings.HasPrefix(e.Encryption, "KMS") {
		p.add(key+".kmsKey", "needs encryption KMS or KMS_DSSE")
	}
}

// validateMutability checks imageTagMutability and its exclusion filters.
func (e ECR) validateMutability(p *problems, key string) {
	if e.ImageTagMutability != "" && !slices.Contains(ecrMutabilities, e.ImageTagMutability) {
		p.add(key+".imageTagMutability", "%q is not one of %s", e.ImageTagMutability, strings.Join(ecrMutabilities, ", "))
	}
	exclusion := strings.HasSuffix(e.ImageTagMutability, "_WITH_EXCLUSION")
	switch {
	case exclusion && len(e.ImageTagMutabilityExclusions) == 0:
		p.add(key+".imageTagMutabilityExclusions", "must name a filter with %s", e.ImageTagMutability)
	case !exclusion && len(e.ImageTagMutabilityExclusions) > 0:
		p.add(key+".imageTagMutabilityExclusions", "needs an imageTagMutability that ends in _WITH_EXCLUSION")
	}
	if slices.Contains(e.ImageTagMutabilityExclusions, "") {
		p.add(key+".imageTagMutabilityExclusions", "must not contain an empty filter")
	}
}

func (r Repo) validate(p *problems, key string) {
	validName(p, key+".name", r.Name)
	if profile.ReservedRepoName(r.Name) {
		p.add(key+".name", "%q %s", r.Name, ReservedRepoName)
	}
	validName(p, key+".format", r.Format)
	switch {
	case r.Kind == KindGroup && r.Registry != "":
		p.add(key+".registry", "a group has none; its members' registries hold what it serves")
	case r.Kind == KindGroup && r.Policy != nil:
		p.add(key+".policy", "a group has none; its members' policies apply")
	case r.Kind != KindGroup && r.Registry == "":
		p.add(key+".registry", "must not be empty")
	}
}

// validateUpstream checks the kind of a repo and its upstream against the configuration: a
// proxy has an upstream that names a configured one of its format, another kind has none; a
// group has a group, another kind has none. The other keys of the upstream (TTLs, offline,
// allow, deny) are the API's to check, when the bootstrap creates the repo
// (docs/spec/proxy-and-group.md §2.5).
func (r Repo) validateUpstream(p *problems, key string, upstreams []Upstream, byName map[string]int) {
	kind := r.Kind
	if kind == "" {
		kind = KindHosted
	}
	if kind != KindGroup && r.Group != nil {
		p.add(key+".group", "only a group repo has one, not a %s repo", kind)
	}
	switch kind {
	case KindHosted, KindGroup:
		if r.Upstream != nil {
			p.add(key+".upstream", "only a proxy repo has one, not a %s repo", kind)
		}
		return
	case KindProxy:
	default:
		p.add(key+".kind", "%q is not %s, %s or %s", r.Kind, KindHosted, KindProxy, KindGroup)
		return
	}
	if r.Upstream == nil {
		p.add(key+".upstream", "a proxy repo must name one of upstreams[].name")
		return
	}
	name, ok := r.Upstream["name"].(string)
	i, configured := byName[name]
	switch {
	case !ok || name == "":
		p.add(key+".upstream.name", "must be one of upstreams[].name")
	case !configured:
		p.add(key+".upstream.name", "%q is not a configured upstream", name)
	case upstreams[i].Format != r.Format && r.Format != "":
		p.add(key+".upstream.name", "%q serves %s, not %s", name, upstreams[i].Format, r.Format)
	}
}

// maxMembers bounds group.members (docs/spec/proxy-and-group.md §2.3).
const maxMembers = 16

// validateMembers checks the members of the group r against the repos of the configuration
// (byID: <format>/<name> to its index): a list of 1 to 16 names, none twice, none a group and at
// most one hosted among those the configuration declares. A member it does not declare may be
// in the control plane already: the bootstrap checks it there, as it checks merge.
func (r Repo) validateMembers(p *problems, key string, repos []Repo, byID map[string]int) {
	raw, ok := r.Group["members"].([]any)
	if r.Group == nil || !ok || len(raw) == 0 || len(raw) > maxMembers {
		p.add(key+".group.members", "must name 1 to %d repos of format %s", maxMembers, r.Format)
		return
	}
	seen, hosted := map[string]int{}, -1
	for i, v := range raw {
		field := fmt.Sprintf("%s.group.members[%d]", key, i)
		name, ok := v.(string)
		if !ok || name == "" {
			p.add(field, "must be the name of a repo")
			continue
		}
		if first, dup := seen[name]; dup {
			p.add(field, "%q is already group.members[%d]", name, first)
			continue
		}
		seen[name] = i
		if j, declared := byID[r.Format+"/"+name]; declared {
			hosted = memberKind(p, field, name, repos[j].Kind, i, hosted)
		}
	}
}

// memberKind checks the kind of the declared member name, the i-th, and returns the index of
// the hosted member so far: hosted, or i when it is the first.
func memberKind(p *problems, field, name, kind string, i, hosted int) int {
	switch {
	case kind == KindGroup:
		p.add(field, "%q is a group; a group is not a member", name)
	case kind != "" && kind != KindHosted:
	case hosted >= 0:
		p.add(field, "%q is a second hosted member, after group.members[%d]", name, hosted)
	default:
		return i
	}
	return hosted
}

func (u Upstream) validate(p *problems, key string) {
	validName(p, key+".name", u.Name)
	validName(p, key+".format", u.Format)
	// The URL is never quoted in errors, as a registry's.
	switch v, err := url.Parse(u.URL); {
	case err != nil || !isHTTP(v):
		p.add(key+".url", "must be an absolute http(s) URL")
	case v.User != nil:
		p.add(key+".url", "must not contain credentials; use auth")
	case v.RawQuery != "" || v.ForceQuery || v.Fragment != "":
		p.add(key+".url", "must not have a query or a fragment")
	}
	if u.Timeout < 0 {
		p.add(key+".timeout", "must not be negative")
	}
	if u.MaxMetadataSize < 0 {
		p.add(key+".maxMetadataSize", "must not be negative")
	}
	a := u.Auth
	switch {
	case a.TokenFile != "" && (a.Username != "" || a.PasswordFile != ""):
		p.add(key+".auth", "tokenFile is a bearer token: not with username and passwordFile")
	case (a.Username == "") != (a.PasswordFile == ""):
		p.add(key+".auth", "username and passwordFile must be set together")
	}
}

// Insecure reports whether the upstream is reached over plain http, which serve warns about:
// what it serves is not authenticated.
func (u Upstream) Insecure() bool {
	v, err := url.Parse(u.URL)
	return err == nil && v.Scheme == "http"
}

func validName(p *problems, key, name string) {
	switch {
	case name == "":
		p.add(key, "must not be empty")
	case len(name) > MaxNameLen || !RepoNamePattern.MatchString(name):
		p.add(key, "%q must be lowercase letters and digits separated by single '.', '_' or '-', at most %d characters", name, MaxNameLen)
	}
}

// validateTokenRealmHosts checks that each of tokenRealmHosts is a host name or an IP address.
func (r Registry) validateTokenRealmHosts(p *problems, key string) {
	for i, h := range r.TokenRealmHosts {
		if !validHost(h) {
			p.add(fmt.Sprintf("%s.tokenRealmHosts[%d]", key, i), "%q is not a host name or an IP address: no scheme, port, path or wildcard", h)
		}
	}
}

// hostLabel is one label of a DNS host name: letters, digits and inner hyphens, at most 63.
var hostLabel = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// validHost reports whether h is what the host of a URL may be, without a port: an IP address
// without a zone, or a host name of labels separated by single dots, at most 253 characters,
// without a root dot.
func validHost(h string) bool {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Zone() == ""
	}
	if h == "" || len(h) > 253 {
		return false
	}
	for l := range strings.SplitSeq(h, ".") {
		if !hostLabel.MatchString(l) {
			return false
		}
	}
	return true
}

func isHTTP(u *url.URL) bool {
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (a Auth) validate(p *problems) {
	if a.Bootstrap.TTL <= 0 {
		p.add("auth.bootstrap.ttl", "must be positive")
	}
}
