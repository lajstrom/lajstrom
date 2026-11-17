package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/ci"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/policy/denylist"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// Options configures the handler New returns.
type Options struct {
	// Repos is the service behind /api/v1/repos; required.
	Repos *Repos
	// Tokens is the service behind the tokens of a user and the deploy tokens of a repo; nil
	// means one over the index, the audit writer, the clock and the logger of Repos, with
	// ServiceTokenMaxLifetime and DeployTokenMaxLifetime.
	Tokens *auth.Tokens
	// ServiceTokenMaxLifetime bounds the lifetime of a service account's personal access tokens,
	// auth.serviceAccounts.maxTokenTTL (auth.Tokens.ServiceMaxLifetime), when Tokens is nil;
	// zero bounds nothing.
	ServiceTokenMaxLifetime time.Duration
	// DeployTokenMaxLifetime bounds the lifetime of a deploy token, of a repo and over a mask,
	// auth.deployTokens.maxTTL (auth.Tokens.DeployMaxLifetime), when Tokens is nil; zero bounds
	// nothing.
	DeployTokenMaxLifetime time.Duration
	// Users is the service behind /api/v1/users; nil means one over the index, the audit
	// writer, the clock and the logger of Repos, with ServiceDisableWithOwner.
	Users *auth.Users
	// ServiceDisableWithOwner disables the enabled service accounts of a user disabled through
	// the API, auth.serviceAccounts.onOwnerDisabled disable (auth.Users.DisableWithOwner), when
	// Users is nil.
	ServiceDisableWithOwner bool
	// InactiveUsersAfter is auth.disableInactiveUsersAfter (auth.Users.InactiveAfter), which the
	// users' inactive answers, when Users is nil; zero marks nobody.
	InactiveUsersAfter time.Duration
	// PasswordsOff is auth.localPasswords false (auth.Users.PasswordsOff), when Users is nil: a
	// password of POST /api/v1/users and POST …/password is 400, naming the setting.
	PasswordsOff bool
	// Lockout is auth.lockout.duration (auth.Users.Lockout), when Users is nil: a locked user is
	// answered with lockedUntil while it is not 0.
	Lockout time.Duration
	// PasswordPolicy is auth.passwords (auth.Users.PasswordPolicy), when Users is nil: the rules
	// of every password POST /api/v1/users and POST …/password set, and with changeAtFirstUse the
	// mustChange they set.
	PasswordPolicy auth.PasswordPolicy
	// Authenticate runs the authentication of the request's credentials that the server's
	// middleware leaves to POST /api/v1/users/{user}/password, whose body decides whether it
	// takes any (server.Server.Authenticate): the admin's new password does, the user's own
	// change does not. It returns the request with its principal, or false when it answered the
	// refusal itself. Nil takes the request as it is, its principal the middleware's.
	Authenticate func(w http.ResponseWriter, r *http.Request) (*http.Request, bool)
	// Bindings is the service behind /api/v1/bindings; nil means one over the index, the
	// formats, the audit writer, the clock and the logger of Repos.
	Bindings *auth.Bindings
	// Auth decides whether the request's principal may perform an action on a repo or on the
	// system (auth.System): serve sets auth.RBAC. nil allows everything the credential allows,
	// for the tests of the handlers.
	Auth auth.Authorizer
	// Reindex starts the runs of POST …/reindex; nil answers it 501.
	Reindex Reindexer
	// Deleter deletes the versions of DELETE …/packages/{name}/versions/{version}: the ingest
	// pipeline; nil answers it 501.
	Deleter Deleter
	// Prefetcher fetches the versions of POST …/prefetch into a proxy repo: the proxy core; nil
	// answers it 501.
	Prefetcher Prefetcher
	// Journal writes the journal ops of a proxy that becomes a hosted repo (PATCH kind) and the
	// yanks of PUT and DELETE …/versions/{version}/yank: the ingest pipeline; nil answers them
	// 501.
	Journal Journaler
	// Bootstrap exchanges the bootstrap token of POST /api/v1/bootstrap for the first admin;
	// nil answers it 404, as once an admin exists.
	Bootstrap *auth.Bootstrap
	// Webhooks is the service behind the webhooks of a repo and of /api/v1/webhooks
	// (docs/spec/webhooks.md); nil answers them 501 once the request may manage them.
	Webhooks *webhook.Service
	// Providers reads the manifests and referrers of GET …/versions/{version}/manifest and
	// …/referrers from the registry of a repo (docs/spec/http-routing.md §4, #951); nil answers
	// them 501.
	Providers provider.Set
	// MaxManifestSize is the largest manifest GET …/versions/{version}/manifest reads, in bytes
	// (server.maxManifestSize); zero or negative means 4 MiB.
	MaxManifestSize int64
	// Rules is the service behind /api/v1/policy/rules (docs/spec/policy.md §7); nil answers
	// them 501.
	Rules *Rules
	// Verdicter attaches the verdicts of a deny: the ingest pipeline; nil answers a deny 501.
	Verdicter Verdicter
	// PolicyKey is the server's policy key (docs/spec/policy.md §5); nil answers a deny 501 and
	// GET /api/v1/policy/key 404.
	PolicyKey PolicyKey
	// Locate gives a version's own download path for the deny list (format.Locator); nil gives
	// none.
	Locate denylist.Locate
	// CIPolicies is the service behind /api/v1/ci/policies (docs/spec/rbac.md §11); nil answers
	// them 501 to the admin role.
	CIPolicies *ci.Service
	// Info is what GET /api/v1/info answers of the configuration and the plugins; the zero
	// value answers an empty externalURL, anonymous read off and no formats.
	Info ServerInfo
	// Passwords checks the password of POST /api/v1/login: the server's authenticator; nil
	// answers a password sign-in 501.
	Passwords Passwords
	// LoginLifetime is auth.login.tokenLifetime, the lifetime of the token of POST
	// /api/v1/login; zero means DefaultLoginLifetime.
	LoginLifetime time.Duration
	// LoginIdle is auth.login.idleTimeout, which GET /api/v1/info answers so that a web UI warns
	// before the server refuses an idle sign-in (docs/spec/rbac.md §10, #1277); zero is off.
	LoginIdle time.Duration
	// Handoffs redeems the codes of OIDC logins of POST /api/v1/login (internal/auth/oidc); nil
	// while auth.oidc is off, when every code is unknown.
	Handoffs Handoffs
	// DPoP checks the proof of POST /api/v1/login, whose key the token is bound to
	// (docs/spec/rbac.md §10): serve's dpop.Verifier, the server's too. nil answers a sign-in 501.
	DPoP Proofs
	// Registries is what GET /api/v1/registries answers of the registries; nil answers none.
	Registries RegistryReporter
	// RequireReason is auth.requireReason (docs/spec/rbac.md §13): a change of the control plane
	// without changeReason is 400. False takes a reason when one is given.
	RequireReason bool
	// Log receives the causes of the 5xx answers, with the request id; nil means
	// slog.Default().
	Log *slog.Logger
}

// maxBody is the largest request body the API reads; a larger one is answered 413.
const maxBody = 64 << 10

// New returns the handler of the management API, which the server mounts at /api/
// (server.Options.API). Its routes (docs/api.md):
//
//	GET    /api/v1/repos[?format=<f>]                  list the repos
//	POST   /api/v1/repos                               create a repo
//	GET    /api/v1/repos/{format}/{repo}               one repo
//	PATCH  /api/v1/repos/{format}/{repo}               change anonymousRead, policy and a proxy's upstream;
//	                                                   {"kind": "hosted"} alone turns a proxy into a hosted repo
//	DELETE /api/v1/repos/{format}/{repo}[?force=true]  delete a repo
//	POST   /api/v1/repos/{format}/{repo}/reindex       start a reindex of the repo: 202, 409 when one runs
//	GET    /api/v1/repos/{format}/{repo}/reindex       the latest run that covered the repo, with its report
//	GET    /api/v1/repos/{format}/{repo}/packages[?prefix=&limit=&cursor=]  the repo's packages by name, a page at a time
//	GET    /api/v1/repos/{format}/{repo}/packages/{name}/versions[?limit=&cursor=]  the package's versions, newest first
//	GET    /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}[?variant=]  one version, its variants and their files
//	GET    /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/attachments[?variant=&kind=&predicateType=&verified=&limit=]
//	                                                   its signatures, SBOMs, scans and attestations with their labels
//	GET    /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/manifest[?variant=]  its variants' manifests,
//	                                                   read from the registry; ETag and If-None-Match for one
//	GET    /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/referrers[?variant=&artifactType=]
//	                                                   its variants' referrers, as the registry lists them
//	DELETE /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}[?variant=&force=]  delete a version, its referrers first;
//	                                                   If-Match and If-Unmodified-Since make it conditional
//	PUT    /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/yank  yank a pypi version, {"reason"} optional
//	DELETE /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/yank  unyank it
//	POST   /api/v1/repos/{format}/{repo}/versions:delete  delete versions in a batch, each conditional; dryRun
//	DELETE /api/v1/repos/{format}/{repo}/upstream-cache[?name=]  drop a proxy's cache of its upstream, or of one name
//	POST   /api/v1/repos/{format}/{repo}/prefetch      fetch versions of 1 to 100 names into a proxy repo, a result each
//	GET    /api/v1/repos/{format}/{repo}/sessions[?limit=&cursor=]  its open publication sessions, maven deploys (manage)
//	GET    /api/v1/repos/{format}/{repo}/deploy-tokens       the deploy tokens of a repo
//	POST   /api/v1/repos/{format}/{repo}/deploy-tokens       create one; the answer has its secret
//	DELETE /api/v1/repos/{format}/{repo}/deploy-tokens/{id}  revoke one
//	GET    /api/v1/deploy-tokens[?repoMask=]           the deploy tokens over a mask (admin)
//	POST   /api/v1/deploy-tokens                       create one over the body's repoMask; the answer has its secret
//	DELETE /api/v1/deploy-tokens/{id}                  revoke one
//	GET    /api/v1/tokens[?kind=&user=&repo=&repoMask=&live=&limit=&cursor=]  every token, a page at a time (admin)
//	GET    /api/v1/repos/{format}/{repo}/webhooks          the webhooks whose mask is the repo
//	POST   /api/v1/repos/{format}/{repo}/webhooks          create one; the answer has its secret
//	GET    /api/v1/repos/{format}/{repo}/webhooks/{id}     one webhook
//	PATCH  /api/v1/repos/{format}/{repo}/webhooks/{id}     change its url, events, active, description
//	DELETE /api/v1/repos/{format}/{repo}/webhooks/{id}     delete it and its delivery log
//	POST   /api/v1/repos/{format}/{repo}/webhooks/{id}/secret      a new secret, answered once
//	POST   /api/v1/repos/{format}/{repo}/webhooks/{id}/test        send a ping now, answer its delivery
//	GET    /api/v1/repos/{format}/{repo}/webhooks/{id}/deliveries[?limit=&cursor=]  its delivery log, newest first
//	…      /api/v1/webhooks…                           the same for every mask, the admin role
//	GET    /api/v1/policy/rules[?repo=]                the policy rules: every rule (admin), or those of a repo
//	POST   /api/v1/policy/rules                        create one: manage on the repo of its mask, admin for a wildcard
//	DELETE /api/v1/policy/rules/{id}                   delete one
//	GET    /api/v1/policy/verdicts?digest=&repo=       the verdicts of a manifest or a repo
//	POST   /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/deny[?variant=]  sign and attach a deny verdict
//	DELETE /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/deny[?variant=]  delete the manual verdicts
//	GET    /api/v1/policy/key                          the policy key's public key, PEM, to anyone
//	GET    /api/v1/policy/denylist                     the deny list of the repos the caller may read, as flat JSON
//	GET    /api/v1/users[?limit=&cursor=]              list the live users, a page at a time
//	POST   /api/v1/users                               create a user
//	GET    /api/v1/users/{user}                        one user, by name or uid
//	PATCH  /api/v1/users/{user}                        change its name and disabled
//	DELETE /api/v1/users/{user}                        delete it
//	POST   /api/v1/users/{user}/password               set its password
//	POST   /api/v1/users/{user}/unlock                 end the lock of its password
//	GET    /api/v1/users/{user}/tokens                 the personal access tokens of a user
//	POST   /api/v1/users/{user}/tokens                 create one; the answer has its secret
//	DELETE /api/v1/users/{user}/tokens/{id}            revoke one
//	GET    /api/v1/bindings[?principal=&limit=&cursor=]  list the bindings, a page at a time
//	POST   /api/v1/bindings                            create a binding
//	DELETE /api/v1/bindings/{id}                       delete one
//	GET    /api/v1/ci/policies                         the CI trust policies, of the configuration and of the API (admin)
//	POST   /api/v1/ci/policies                         make one of the API
//	GET    /api/v1/ci/policies/{name}                  one policy
//	PUT    /api/v1/ci/policies/{name}                  replace the issuer, claims, grants and expiry of one of the API
//	DELETE /api/v1/ci/policies/{name}                  delete one of the API
//	POST   /api/v1/ci/policies/{name}/renew            renew one of the API that grants write
//	POST   /api/v1/ci/policies/{name}/approve          approve what waits on one of the API (If-Match)
//	GET    /api/v1/audit[?repo=&principal=&action=&since=&until=&limit=&cursor=]  the audit log, newest first
//	GET    /api/v1/whoami                              the request's principal
//	GET    /api/v1/info                                the server's build, facts and the caller, to anyone
//	GET    /api/v1/registries                          the registries, their capabilities, repos and health (admin)
//	POST   /api/v1/bootstrap                           the first admin, for the bootstrap token; 404 once an admin exists
//	POST   /api/v1/login                               sign in a web UI, a password or an OIDC code, and a DPoP proof: a short-lived token bound to its key
//	GET    /api/v1/openapi.yaml                        the OpenAPI document of these routes
//
// Every other method and path under /api/ is a JSON NOT_FOUND. Answers are JSON;
// errors are the envelope of §4. New panics when o.Repos is nil.
func New(o Options) http.Handler {
	if o.Repos == nil {
		panic("api: New: nil Options.Repos")
	}
	h := &handler{repos: o.Repos, tokens: o.Tokens, users: o.Users, bindings: o.Bindings, auth: o.Auth, reindex: o.Reindex,
		deleter: o.Deleter, prefetcher: o.Prefetcher, journal: o.Journal, boot: o.Bootstrap, info: o.Info, log: o.Log,
		webhooks: o.Webhooks, passwords: o.Passwords, loginLifetime: o.LoginLifetime, loginIdle: o.LoginIdle, handoffs: o.Handoffs, proofs: o.DPoP,
		registries: o.Registries, providers: o.Providers, maxManifest: o.MaxManifestSize,
		rules: o.Rules, verdicter: o.Verdicter, policyKey: o.PolicyKey,
		locate: o.Locate, ci: o.CIPolicies, authenticate: o.Authenticate, requireReason: o.RequireReason}
	if h.loginLifetime <= 0 {
		h.loginLifetime = DefaultLoginLifetime
	}
	if h.maxManifest <= 0 {
		h.maxManifest = defaultMaxManifestSize
	}
	if h.info.Formats == nil {
		h.info.Formats = []FormatInfo{}
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.tokens == nil {
		h.tokens = &auth.Tokens{Index: o.Repos.Index, Formats: o.Repos.Formats, Audit: o.Repos.Audit, Now: o.Repos.Now, Log: h.log,
			ServiceMaxLifetime: o.ServiceTokenMaxLifetime, DeployMaxLifetime: o.DeployTokenMaxLifetime}
	}
	if h.users == nil {
		h.users = &auth.Users{Index: o.Repos.Index, Audit: o.Repos.Audit, Now: o.Repos.Now, Log: h.log,
			DisableWithOwner: o.ServiceDisableWithOwner, InactiveAfter: o.InactiveUsersAfter, PasswordsOff: o.PasswordsOff,
			Lockout: o.Lockout, PasswordPolicy: o.PasswordPolicy}
	}
	if h.bindings == nil {
		h.bindings = &auth.Bindings{Index: o.Repos.Index, Formats: o.Repos.Formats, Audit: o.Repos.Audit, Now: o.Repos.Now, Log: h.log}
	}
	mux := http.NewServeMux()
	for _, rt := range h.routes() {
		mux.Handle(rt.pattern, h.serve(rt.fn))
	}
	mux.Handle("GET /api/v1/openapi.yaml", http.HandlerFunc(serveOpenAPI))
	mux.Handle("/api/", h.serve(h.notFound))
	return mux
}

// route is a method and path of the API, as http.ServeMux takes them, and its endpoint.
type route struct {
	pattern string
	fn      endpoint
}

// routes are the endpoints of the API: what New serves and what internal/api/openapi.yaml
// describes (TestOpenAPIRoutes holds the two to each other).
func (h *handler) routes() []route {
	return []route{
		{"GET /api/v1/repos", h.listRepos},
		{"POST /api/v1/repos", h.createRepo},
		{"GET /api/v1/repos/{format}/{repo}", h.getRepo},
		{"PATCH /api/v1/repos/{format}/{repo}", h.updateRepo},
		{"DELETE /api/v1/repos/{format}/{repo}", h.deleteRepo},
		{"POST /api/v1/repos/{format}/{repo}/reindex", h.reindexRepo},
		{"GET /api/v1/repos/{format}/{repo}/reindex", h.reindexStatus},
		{"GET /api/v1/repos/{format}/{repo}/packages", h.listPackages},
		{"GET /api/v1/repos/{format}/{repo}/packages/{name}", h.getPackage},
		{"GET /api/v1/repos/{format}/{repo}/packages/{name}/versions", h.listVersions},
		{"GET /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}", h.getVersion},
		{"DELETE /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}", h.deleteVersion},
		{"PUT /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/yank", h.yankVersion},
		{"DELETE /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/yank", h.unyankVersion},
		{"GET /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/attachments", h.listAttachments},
		{"GET /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/manifest", h.getManifests},
		{"GET /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/referrers", h.listReferrers},
		{"POST /api/v1/repos/{format}/{repo}/versions:delete", h.deleteVersions},
		{"DELETE /api/v1/repos/{format}/{repo}/upstream-cache", h.purgeUpstreamCache},
		{"POST /api/v1/repos/{format}/{repo}/prefetch", h.prefetch},
		{"GET /api/v1/repos/{format}/{repo}/sessions", h.listSessions},
		{"GET /api/v1/repos/{format}/{repo}/deploy-tokens", h.listDeployTokens},
		{"POST /api/v1/repos/{format}/{repo}/deploy-tokens", h.createDeployToken},
		{"DELETE /api/v1/repos/{format}/{repo}/deploy-tokens/{id}", h.revokeDeployToken},
		{"GET /api/v1/deploy-tokens", h.listMaskTokens},
		{"POST /api/v1/deploy-tokens", h.createMaskToken},
		{"DELETE /api/v1/deploy-tokens/{id}", h.revokeMaskToken},
		{"GET /api/v1/tokens", h.listAllTokens},
		{"GET /api/v1/policy/rules", h.listRules},
		{"POST /api/v1/policy/rules", h.createRule},
		{"DELETE /api/v1/policy/rules/{id}", h.deleteRule},
		{"GET /api/v1/policy/verdicts", h.listVerdicts},
		{"GET /api/v1/policy/key", h.policyKeyPEM},
		{"GET /api/v1/policy/denylist", h.denylist},
		{"POST /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/deny", h.denyVersion},
		{"DELETE /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/deny", h.undenyVersion},
		{"GET /api/v1/repos/{format}/{repo}/webhooks", h.listWebhooks},
		{"POST /api/v1/repos/{format}/{repo}/webhooks", h.createWebhook},
		{"GET /api/v1/repos/{format}/{repo}/webhooks/{id}", h.getWebhook},
		{"PATCH /api/v1/repos/{format}/{repo}/webhooks/{id}", h.updateWebhook},
		{"DELETE /api/v1/repos/{format}/{repo}/webhooks/{id}", h.deleteWebhook},
		{"POST /api/v1/repos/{format}/{repo}/webhooks/{id}/secret", h.rotateWebhookSecret},
		{"POST /api/v1/repos/{format}/{repo}/webhooks/{id}/test", h.testWebhook},
		{"GET /api/v1/repos/{format}/{repo}/webhooks/{id}/deliveries", h.listDeliveries},
		{"GET /api/v1/webhooks", h.listWebhooks},
		{"POST /api/v1/webhooks", h.createWebhook},
		{"GET /api/v1/webhooks/{id}", h.getWebhook},
		{"PATCH /api/v1/webhooks/{id}", h.updateWebhook},
		{"DELETE /api/v1/webhooks/{id}", h.deleteWebhook},
		{"POST /api/v1/webhooks/{id}/secret", h.rotateWebhookSecret},
		{"POST /api/v1/webhooks/{id}/test", h.testWebhook},
		{"GET /api/v1/webhooks/{id}/deliveries", h.listDeliveries},
		{"GET /api/v1/users", h.listUsers},
		{"POST /api/v1/users", h.createUser},
		{"GET /api/v1/users/{user}", h.getUser},
		{"PATCH /api/v1/users/{user}", h.updateUser},
		{"DELETE /api/v1/users/{user}", h.deleteUser},
		{"POST /api/v1/users/{user}/password", h.setPassword},
		{"POST /api/v1/users/{user}/unlock", h.unlockUser},
		{"GET /api/v1/users/{user}/tokens", h.listTokens},
		{"POST /api/v1/users/{user}/tokens", h.createToken},
		{"DELETE /api/v1/users/{user}/tokens/{id}", h.revokeToken},
		{"GET /api/v1/bindings", h.listBindings},
		{"POST /api/v1/bindings", h.createBinding},
		{"DELETE /api/v1/bindings/{id}", h.deleteBinding},
		{"GET /api/v1/ci/policies", h.listCIPolicies},
		{"POST /api/v1/ci/policies", h.createCIPolicy},
		{"GET /api/v1/ci/policies/{name}", h.getCIPolicy},
		{"PUT /api/v1/ci/policies/{name}", h.replaceCIPolicy},
		{"DELETE /api/v1/ci/policies/{name}", h.deleteCIPolicy},
		{"POST /api/v1/ci/policies/{name}/renew", h.renewCIPolicy},
		{"POST /api/v1/ci/policies/{name}/approve", h.approveCIPolicy},
		{"GET /api/v1/audit", h.listAudit},
		{"GET /api/v1/whoami", h.whoami},
		{"GET /api/v1/info", h.serverInfo},
		{"GET /api/v1/search", h.search},
		{"GET /api/v1/registries", h.listRegistries},
		{"POST /api/v1/bootstrap", h.bootstrap},
		{"POST /api/v1/login", h.login},
	}
}

// handler serves the management API.
type handler struct {
	repos    *Repos
	tokens   *auth.Tokens
	users    *auth.Users
	bindings *auth.Bindings
	auth     auth.Authorizer
	reindex  Reindexer
	deleter  Deleter
	boot     *auth.Bootstrap
	// prefetcher and journal are Options.Prefetcher and Options.Journal.
	prefetcher Prefetcher
	journal    Journaler
	// info is Options.Info, its Formats never nil.
	info ServerInfo
	// webhooks is Options.Webhooks, ci Options.CIPolicies.
	webhooks *webhook.Service
	// passwords, loginLifetime, loginIdle, handoffs and proofs are Options.Passwords,
	// Options.LoginLifetime, never zero, Options.LoginIdle, Options.Handoffs and Options.DPoP.
	passwords     Passwords
	loginLifetime time.Duration
	loginIdle     time.Duration
	handoffs      Handoffs
	proofs        Proofs
	// authenticate is Options.Authenticate.
	authenticate func(w http.ResponseWriter, r *http.Request) (*http.Request, bool)
	// registries is Options.Registries.
	registries RegistryReporter
	// providers is Options.Providers; maxManifest is Options.MaxManifestSize, 4 MiB when it was
	// not positive.
	providers   provider.Set
	maxManifest int64
	// rules, verdicter and policyKey are Options.Rules, Options.Verdicter and Options.PolicyKey.
	rules     *Rules
	verdicter Verdicter
	policyKey PolicyKey
	locate    denylist.Locate
	// ci is Options.CIPolicies.
	ci *ci.Service
	// requireReason is Options.RequireReason.
	requireReason bool
	log           *slog.Logger
}

// endpoint is a handler that returns its answer: the status and the value to encode as JSON
// (nil for no body), or an error, which serve answers with the envelope.
type endpoint func(w http.ResponseWriter, r *http.Request) (status int, body any, err error)

// serve adapts an endpoint to http.Handler.
func (h *handler) serve(fn endpoint) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body, err := fn(w, r)
		switch {
		case err != nil:
			h.fail(w, r, err)
		case body == nil && status == 0: // the endpoint wrote its answer
		case body == nil:
			w.WriteHeader(status)
		default:
			h.writeJSON(w, r, status, body)
		}
	})
}

// listRepos answers the repos the principal may read, of the format of ?format or of every
// format, as {"repos": […]} ordered by format and name, each with what the request may do on it
// and what the index holds of it (withFacts).
func (h *handler) listRepos(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	repos, err := h.repos.List(r.Context(), r.URL.Query().Get("format"))
	if err != nil {
		return 0, nil, err
	}
	if repos, err = h.readable(r, repos); err != nil {
		return 0, nil, err
	}
	if err := h.withFacts(r, repos); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, RepoList{Repos: repos}, nil
}

// readable keeps the repos the principal may read: read lists a repo in /api/v1/repos
// (docs/spec/rbac.md §2). A denial leaves the repo out, so that the list does not tell which
// repos exist; an Authorizer that cannot decide fails the whole list.
func (h *handler) readable(r *http.Request, repos []Repo) ([]Repo, error) {
	kept := repos[:0]
	for _, repo := range repos {
		switch err := h.allow(r, auth.ActionRead, repo.Format, repo.Name); {
		case err == nil:
			kept = append(kept, repo)
		case !denied(err):
			return nil, err
		}
	}
	return kept, nil
}

// denied reports whether err is a denial of allow rather than a failure to decide: a sign-in
// asked for more (auth.StepUpError) among them.
func denied(err error) bool {
	var stepUp *auth.StepUpError
	return errors.Is(err, format.ErrUnauthenticated) || errors.Is(err, format.ErrForbidden) ||
		errors.Is(err, index.ErrNotFound) || errors.As(err, &stepUp)
}

// createRepo creates the repo of the CreateRequest in the body: 201 with the resource and its
// Location. The admin role is checked before the body is read, and must stay first: the 409 of
// a taken name, and the 400 of a body that does not parse, then reach an admin only, who sees
// every repo, and nobody else learns by trying names which repos exist (docs/spec/rbac.md §3).
func (h *handler) createRepo(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allow(r, auth.ActionManage, auth.System, auth.System); err != nil {
		return 0, nil, err
	}
	var req CreateRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	o, err := h.changeOrigin(r, req.ChangeReason)
	if err != nil {
		return 0, nil, err
	}
	repo, err := h.repos.Create(r.Context(), req, o)
	if err != nil {
		return 0, nil, err
	}
	w.Header().Set("Location", "/api/v1/repos/"+repo.Format+"/"+repo.Name)
	w.Header().Set("ETag", repo.ETag())
	return http.StatusCreated, repo, nil
}

// getRepo answers one repo, with what the request may do on it and what the index holds of it
// (withFacts), and with its ETag; 304 for an If-None-Match that has it.
func (h *handler) getRepo(w http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionRead, f, name); err != nil {
		return 0, nil, err
	}
	repo, err := h.repos.Get(r.Context(), f, name)
	if err != nil {
		return 0, nil, err
	}
	one := []Repo{*repo}
	if err := h.withFacts(r, one); err != nil {
		return 0, nil, err
	}
	return tagged(w, r, one[0].ETag(), one[0])
}

// updateRepo applies the UpdateRequest of the body and answers the repo as it is now, with its
// ETag. With If-Match, the change is made only while the repo has a revision it names.
func (h *handler) updateRepo(w http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	r, err := conditional(r, TableRepos, "repo "+f+"/"+name)
	if err != nil {
		return 0, nil, err
	}
	var body patchBody
	if err := decodeBody(w, r, &body); err != nil {
		return 0, nil, err
	}
	if body.Kind != nil {
		status, out, err := h.convertRepo(r, f, name, body)
		if repo, ok := out.(Repo); ok {
			w.Header().Set("ETag", repo.ETag())
		}
		return status, out, err
	}
	req, err := body.request()
	if err != nil {
		return 0, nil, err
	}
	// Only the admin role changes upstream.name, group.members and group.paths (docs/spec/rbac.md
	// §7).
	// The role is asked here, outside the transaction of Update, and only when the body has
	// one of them; Update compares the value with the stored one in its transaction and
	// refuses a change without the role, so that sending back the value the repo has is not
	// one.
	if hasKey(req.Upstream, "name") || hasKey(req.Group, "members") || hasKey(req.Group, "paths") {
		switch err := h.allow(r, auth.ActionManage, auth.System, auth.System); {
		case err == nil:
			req.Admin = true
		case !denied(err):
			return 0, nil, err
		}
	}
	o, err := h.changeOrigin(r, body.ChangeReason)
	if err != nil {
		return 0, nil, err
	}
	repo, err := h.repos.Update(r.Context(), f, name, req, o)
	if err != nil {
		return 0, nil, err
	}
	w.Header().Set("ETag", repo.ETag())
	return http.StatusOK, repo, nil
}

// deleteRepo deletes a repo, refusing one with versions unless ?force=true: 204. With If-Match,
// only while the repo has a revision it names.
func (h *handler) deleteRepo(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	r, err := conditional(r, TableRepos, "repo "+f+"/"+name)
	if err != nil {
		return 0, nil, err
	}
	force, err := boolParam(r, "force")
	if err != nil {
		return 0, nil, err
	}
	o, err := h.changeOrigin(r, nil)
	if err != nil {
		return 0, nil, err
	}
	if err := h.repos.Delete(r.Context(), f, name, force, o); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// notFound answers every other method and path under /api/.
func (h *handler) notFound(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	return 0, nil, &clientError{
		msg: fmt.Sprintf("%s %s is not an endpoint of this version (docs/api.md)",
			r.Method, r.URL.Path),
		err: index.ErrNotFound,
	}
}

// allow returns nil when the request's principal may perform a on formatName/repo. It is the
// one place where the API asks the Authorizer, through format.Authorize, so that a denial
// follows docs/spec/rbac.md §3 as on the frontends: the anonymous principal gets
// format.ErrUnauthenticated (401 with a Basic challenge), a principal that can read the repo
// but lacks a gets format.ErrForbidden (403), and one that cannot read it index.ErrNotFound
// (404), the answer of a repo that does not exist. Reading a repo and listing it need read;
// changing, deleting and reindexing it need manage.
//
// Creating a repo is system-level, and asks for manage on auth.System/auth.System, which only
// the admin role has (rbac.md §2): a maintainer on */* has every repo and not the system. An
// authenticated principal that is denied gets 403, never 404: the check comes before the body
// names a repo (createRepo), so no repo's existence is at stake.
//
// The request's credential limits the principal first, with or without an Authorizer
// (format.Authorize, auth.Credential.Limit): a deploy token and a personal access token with
// scopes never get manage, and a deploy token reaches its own repo only.
func (h *handler) allow(r *http.Request, a auth.Action, formatName, repo string) error {
	return h.authorize(r, format.Deps{Auth: h.auth}, a, formatName, repo)
}

// limit is allow without the Authorizer: what the request's credential allows alone.
func (h *handler) limit(r *http.Request, a auth.Action, formatName, repo string) error {
	return h.authorize(r, format.Deps{}, a, formatName, repo)
}

// authorize asks format.Authorize with deps, and answers a principal denied on the system
// with format.ErrForbidden. Under the admin mode (docs/spec/rbac.md §12), a principal denied
// on the system whose admin binding the mode leaves out for the request's credential gets
// auth.AdminMFARefusal instead: a web UI's sign-in through OIDC without a second factor the 401
// that asks for more, any other credential a 403 naming auth.admin.requireMFA and its kind.
func (h *handler) authorize(r *http.Request, deps format.Deps, a auth.Action, formatName, repo string) error {
	ctx := r.Context()
	err := format.Authorize(ctx, deps, a, index.Repo{Format: formatName, Name: repo})
	if formatName != auth.System || repo != auth.System || !denied(err) {
		return err
	}
	if am, ok := deps.Auth.(auth.AdminMode); ok {
		switch left, aerr := am.AdminLeftOut(ctx, auth.PrincipalFrom(ctx)); {
		case aerr != nil:
			return aerr
		case left:
			return serviceError(auth.AdminMFARefusal(auth.CredentialFrom(ctx), time.Now()))
		}
	}
	if errors.Is(err, index.ErrNotFound) {
		return fmt.Errorf("%w: %s on the system", format.ErrForbidden, a)
	}
	return err
}

// patchBody is the body of PATCH: the fields of an UpdateRequest, those that cannot change,
// refused by name rather than as unknown fields, and the reason of the change.
type patchBody struct {
	AnonymousRead *bool           `json:"anonymousRead"`
	Policy        json.RawMessage `json:"policy"`
	Upstream      json.RawMessage `json:"upstream"`
	Group         json.RawMessage `json:"group"`
	Format        json.RawMessage `json:"format"`
	Name          json.RawMessage `json:"name"`
	Registry      json.RawMessage `json:"registry"`
	Kind          json.RawMessage `json:"kind"`
	Reason
}

// request returns the UpdateRequest of b, or an error naming a field that cannot change.
func (b *patchBody) request() (UpdateRequest, error) {
	for _, f := range []struct {
		field, why string
		value      json.RawMessage
	}{
		{"format", "it identifies the repo", b.Format},
		{"name", "it identifies the repo", b.Name},
		{"registry", "the artifacts stay in the registry they were pushed to, and moving the repo would orphan them", b.Registry},
		{"kind", "it is set when the repo is created", b.Kind},
	} {
		if f.value != nil {
			return UpdateRequest{}, invalid("%s: cannot change; %s", f.field, f.why)
		}
	}
	return UpdateRequest{AnonymousRead: b.AnonymousRead, Policy: b.Policy, Upstream: b.Upstream, Group: b.Group}, nil
}

// decodeBody reads the JSON object of the request body into v: at most maxBody bytes, no field
// that v does not have, nothing after the object. Its errors are for the client and name the
// field when there is one; a body that is too large is a *http.MaxBytesError (413), one that
// server.stallTimeout cut is os.ErrDeadlineExceeded (408).
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return bodyError(err)
	}
	switch _, err := dec.Token(); {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return bodyError(err)
	}
	return invalid("body: must be one JSON object, with nothing after it")
}

// decodeOptionalBody is decodeBody for a body that may be left out: an empty body, whether a
// Content-Length of 0 says so or a chunked body ends at once, leaves v as it is.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	br := bufio.NewReader(r.Body)
	switch _, err := br.Peek(1); {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return bodyError(err)
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{br, r.Body}
	return decodeBody(w, r, v)
}

// bodyError turns an error of decodeBody into one for the client. A body that
// server.stallTimeout cut keeps its cause, which format.StatusOf answers 408 (#576), with the
// fixed message of format.ClientMessage: the cause names the connection's addresses (#639).
func bodyError(err error) error {
	var (
		tooLarge *http.MaxBytesError
		typeErr  *json.UnmarshalTypeError
	)
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return &clientError{msg: format.ClientMessage(err), err: err}
	case errors.As(err, &tooLarge):
		return &clientError{msg: fmt.Sprintf("body: larger than %d bytes", maxBody), err: err}
	case errors.Is(err, io.EOF):
		return invalid("body: must be a JSON object")
	case errors.As(err, &typeErr) && typeErr.Field != "":
		return invalid("%s: must not be a JSON %s", typeErr.Field, typeErr.Value)
	case errors.As(err, &typeErr):
		return invalid("body: must be a JSON object, not %s", typeErr.Value)
	}
	return invalid("body: %s", strings.TrimPrefix(err.Error(), "json: "))
}

// boolParam returns the query parameter key as a bool; absent is false.
func boolParam(r *http.Request, key string) (bool, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, invalid("%s: %q is not true or false", key, v)
	}
	return b, nil
}

// origin is the Origin of a change made by a request.
func origin(r *http.Request) Origin {
	return Origin{Source: SourceAPI, IP: clientIP(r), RequestID: server.RequestID(r.Context())}
}

// clientIP is the address of the client without the port: the server's (server.ClientAddr,
// which believes X-Forwarded-For from server.trustedProxies only, ADR 0011), or the peer of the
// connection for a request that did not come through the server's middleware.
func clientIP(r *http.Request) string {
	if addr := server.ClientAddr(r.Context()); addr != "" {
		return addr
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
