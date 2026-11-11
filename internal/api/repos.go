package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// Repo is the repos resource of the management API, as GET /api/v1/repos/{format}/{repo}
// answers it (docs/api.md). Format and Name identify the repo: <format>/<name> is the RBAC
// resource and the OCI path below the registry's instance prefix (ADR 0004). The repo's prefix
// in the index is internal, <format>/<name> in the MVP, and not shown.
type Repo struct {
	Format        string          `json:"format"`
	Name          string          `json:"name"`
	Registry      string          `json:"registry"`
	Kind          string          `json:"kind"` // hosted, proxy or group
	AnonymousRead bool            `json:"anonymousRead"`
	Policy        json.RawMessage `json:"policy"` // a JSON object, or nil (null)
	// Upstream is a proxy repo's settings (docs/spec/proxy-and-group.md §2.2), every key with
	// its value: the upstream's name, never its URL or credentials. Absent on another kind.
	Upstream json.RawMessage `json:"upstream,omitempty"`
	// Group is a group's settings (§2.3): {"members", "merge"}, the members by name in order.
	// Absent on another kind.
	Group     json.RawMessage `json:"group,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// MarshalJSON writes r as the API shows it: a group has neither a registry nor a policy, and
// its resource has neither key (docs/spec/proxy-and-group.md §2.3).
func (r Repo) MarshalJSON() ([]byte, error) {
	type plain Repo
	if r.Kind != index.KindGroup {
		return json.Marshal(plain(r))
	}
	return json.Marshal(struct {
		Format        string          `json:"format"`
		Name          string          `json:"name"`
		Kind          string          `json:"kind"`
		AnonymousRead bool            `json:"anonymousRead"`
		Group         json.RawMessage `json:"group,omitempty"`
		CreatedAt     time.Time       `json:"createdAt"`
		UpdatedAt     time.Time       `json:"updatedAt"`
	}{r.Format, r.Name, r.Kind, r.AnonymousRead, r.Group, r.CreatedAt, r.UpdatedAt})
}

// RepoList is the answer of GET /api/v1/repos and of lajstrom admin repo list --json.
type RepoList struct {
	Repos []Repo `json:"repos"`
}

// CreateRequest is a repo to create: the body of POST /api/v1/repos, and what lajstrom admin
// repo create and the config bootstrap pass to Repos.Create. Kind is empty or hosted, proxy
// with Upstream, the proxy's settings (proxy.ParseSettings), or group with Group, the group's
// members and merge patterns (proxy.ParseGroup) and no Registry. Policy is a JSON object, or
// empty or the JSON null for none, and none on a group; its keys come with the formats.
type CreateRequest struct {
	Format        string          `json:"format"`
	Name          string          `json:"name"`
	Registry      string          `json:"registry"`
	Kind          string          `json:"kind"`
	AnonymousRead bool            `json:"anonymousRead"`
	Policy        json.RawMessage `json:"policy"`
	Upstream      json.RawMessage `json:"upstream"`
	Group         json.RawMessage `json:"group"`
}

// UpdateRequest is a change to a repo, the body of PATCH /api/v1/repos/{format}/{repo}. A nil
// AnonymousRead is left as it is. Policy tells absent from null by its bytes, as
// encoding/json leaves them: nil (absent) leaves the policy, the JSON null clears it, an object
// is merged into it as an RFC 7396 merge patch (mergePolicy). Format, name, registry and kind
// cannot change: moving a repo to another registry would orphan its artifacts.
type UpdateRequest struct {
	AnonymousRead *bool           `json:"anonymousRead"`
	Policy        json.RawMessage `json:"policy"`
	// Upstream is merged into a proxy's settings as Policy is into the policy; nil (absent)
	// leaves them. The settings cannot be removed, and only a proxy has them.
	Upstream json.RawMessage `json:"upstream"`
	// Group is merged into a group's settings, {"members", "merge"}, the same way; each list is
	// replaced whole. Only a group has them.
	Group json.RawMessage `json:"group"`
}

// Sources of a change, the "source" of its audit details.
const (
	// SourceAPI is a request to /api/v1.
	SourceAPI = "api"
	// SourceCLI is lajstrom admin repo.
	SourceCLI = "cli"
	// SourceConfig is the config bootstrap at start (Repos.Bootstrap).
	SourceConfig = "config"
)

// Origin says where a change comes from, for its audit event. The principal is not part of it:
// it is auth.PrincipalFrom(ctx): the request's, or anonymous for the CLI and the config bootstrap.
type Origin struct {
	// Source is SourceAPI, SourceCLI or SourceConfig; the audit details carry it as "source".
	Source string
	// IP is the client address without the port; empty outside HTTP.
	IP string
	// RequestID is the id of the HTTP request (server.RequestID); the audit details carry it as
	// "requestId" when it is set.
	RequestID string
	// Host is the host name of the machine lajstrom admin ran on; the audit details carry it as
	// "host" when it is set.
	Host string
}

// Audit actions of the repo changes (docs/spec/index-schema.md, audit).
const (
	actionCreate = "repo.create"
	actionUpdate = "repo.update"
	actionDelete = "repo.delete"
)

// formatOCI is the format of the pass-through repos: the OCI facade stores what it gets and
// indexes nothing, so no plugin is needed (format.Reserved has it).
const formatOCI = "oci"

// RepoFormat reports whether a repo may have the format name on this server: a format plugin
// compiled in, or oci. lajstrom admin binding checks the format of a mask with it.
func RepoFormat(name string) bool {
	if name == formatOCI {
		return true
	}
	_, ok := format.Lookup(name)
	return ok
}

// Repos manages the repos of the control plane (docs/spec/index-schema.md, repos). The handlers
// of /api/v1/repos, lajstrom admin repo and the config bootstrap all go through it, so that the
// validation and the audit events exist once. Every change is one audit event
// (docs/spec/rbac.md §5) by auth.PrincipalFrom(ctx). Create it as a struct literal; its methods
// may be called concurrently.
type Repos struct {
	// Index is the index database; required.
	Index index.Store
	// Formats reports whether a format plugin of that name is compiled in; nil means
	// format.Lookup.
	Formats func(name string) bool
	// Registries reports whether a registry of that name is configured; nil means none is.
	Registries func(name string) bool
	// Upstreams returns the format of the configured upstream of that name (upstreams[]) and
	// whether there is one; nil means none is configured.
	Upstreams func(name string) (formatName string, ok bool)
	// Proxiers returns the proxy hooks of a format's plugin, and whether it has proxy repos;
	// nil means format.Lookup and the plugin's format.Proxier.
	Proxiers func(formatName string) (format.Proxier, bool)
	// Groupers reports whether a format's plugin has group repos; nil means format.Lookup and
	// the plugin's format.Grouper.
	Groupers func(formatName string) bool
	// Audit records the events; nil means each event goes to the index in the transaction of
	// its change.
	Audit audit.Writer
	// Now is the clock of createdAt and of the audit events; nil means time.Now. The index sets
	// updatedAt of an update itself.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// Create validates req and creates the repo with one repo.create audit event, whose details are
// the resource and the origin. Each validation error wraps format.ErrInvalid and names the
// field: format a compiled-in plugin or oci, name the grammar of docs/spec/manifest-profile.md §4.1
// (at most 64 bytes), registry a configured one, kind empty, hosted or proxy, upstream on a
// proxy only (checkKind), policy null or a JSON object that checkPolicy takes. A repo that
// exists already is an error wrapping index.ErrConflict.
func (s *Repos) Create(ctx context.Context, req CreateRequest, o Origin) (*Repo, error) {
	row, group, err := s.newRow(req)
	if err != nil {
		return nil, err
	}
	var (
		ev index.AuditEvent
		r  Repo
	)
	err = s.Index.Tx(ctx, func(w index.Writer) (err error) {
		var members []int64
		if group != nil {
			if members, err = checkMembers(ctx, w, req.Format, group.Members); err != nil {
				return err
			}
		}
		if err = w.CreateRepo(ctx, &row); err != nil {
			return err
		}
		if group != nil {
			if err = w.SetGroupMembers(ctx, row.ID, members); err != nil {
				return err
			}
		}
		if r, err = resource(ctx, w, &row); err != nil {
			return err
		}
		ev, err = s.record(ctx, w, actionCreate, &row, o, map[string]any{"repo": r})
		return err
	})
	if err != nil {
		return nil, repoError("create", req.Format, req.Name, err)
	}
	s.writeAudit(ctx, ev)
	return &r, nil
}

// Get returns the repo formatName/name, or an error wrapping index.ErrNotFound.
func (s *Repos) Get(ctx context.Context, formatName, name string) (*Repo, error) {
	row, err := s.Index.GetRepo(ctx, formatName, name)
	if err != nil {
		return nil, repoError("get", formatName, name, err)
	}
	r, err := resource(ctx, s.Index, row)
	if err != nil {
		return nil, repoError("get", formatName, name, err)
	}
	return &r, nil
}

// List returns the repos of formatName, or of every format when it is "", ordered by format and
// name. A formatName that is not a compiled-in plugin is an error wrapping format.ErrInvalid.
func (s *Repos) List(ctx context.Context, formatName string) ([]Repo, error) {
	if formatName != "" {
		if err := s.checkFormat(formatName); err != nil {
			return nil, err
		}
	}
	rows, err := s.Index.ListRepos(ctx, formatName)
	if err != nil {
		return nil, fmt.Errorf("api: list repos: %w", err)
	}
	repos := make([]Repo, len(rows))
	for i := range rows {
		if repos[i], err = resource(ctx, s.Index, &rows[i]); err != nil {
			return nil, fmt.Errorf("api: list repos: %w", err)
		}
	}
	return repos, nil
}

// Update applies req to the repo formatName/name in one transaction with one repo.update audit
// event, whose details name the fields that changed with their new values. A request that
// changes nothing writes nothing and returns the repo as it is. The policy of req is merged
// into the repo's (mergePolicy) and the result checked as a whole; so is the upstream of a
// proxy, and a change of its name drops the repo's cache of the old upstream (proxy_meta) and
// its rendered documents in the same transaction (docs/spec/proxy-and-group.md §2.2). Errors
// wrap format.ErrInvalid (a policy or an upstream that is not an object, a merge that is
// refused, an upstream on a repo that is not a proxy) or index.ErrNotFound.
func (s *Repos) Update(ctx context.Context, formatName, name string, req UpdateRequest, o Origin) (*Repo, error) {
	if _, err := decodePolicy(req.Policy); err != nil {
		return nil, err
	}
	var (
		row *index.Repo
		ev  *index.AuditEvent
	)
	err := s.Index.Tx(ctx, func(w index.Writer) (err error) {
		if row, err = w.GetRepo(ctx, formatName, name); err != nil {
			return err
		}
		changed, err := s.change(ctx, w, row, req)
		if err != nil || len(changed) == 0 {
			return err
		}
		e, err := s.record(ctx, w, actionUpdate, row, o, map[string]any{"changed": changed})
		ev = &e
		return err
	})
	if err != nil {
		return nil, repoError("update", formatName, name, err)
	}
	if ev != nil {
		s.writeAudit(ctx, *ev)
	}
	r, err := resource(ctx, s.Index, row)
	if err != nil {
		return nil, repoError("update", formatName, name, err)
	}
	return &r, nil
}

// change applies req to row on w and returns the fields that changed, with their new values,
// having written them: the repos row; a group's members, which change with it; and, for a
// proxy whose upstream is renamed, the drop of its cache of the old one. Nothing changed writes
// nothing.
func (s *Repos) change(ctx context.Context, w index.Writer, row *index.Repo, req UpdateRequest) (map[string]any, error) {
	changed, err := applyUpdate(row, req)
	if err != nil {
		return nil, err
	}
	renamed := false
	if req.Upstream != nil {
		if renamed, err = s.applyUpstream(row, req.Upstream, changed); err != nil {
			return nil, err
		}
	}
	var members []int64
	if req.Group != nil {
		if members, err = applyGroup(ctx, w, row, req.Group, changed); err != nil {
			return nil, err
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	if err = w.UpdateRepo(ctx, row); err != nil {
		return nil, err
	}
	if members != nil {
		if err = w.SetGroupMembers(ctx, row.ID, members); err != nil {
			return nil, err
		}
	}
	if renamed {
		if err := dropUpstreamCache(ctx, w, row.ID, ""); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

// applyUpdate changes row as req says and returns the fields that changed, with their new
// values: the policy as the merge left it.
func applyUpdate(row *index.Repo, req UpdateRequest) (map[string]any, error) {
	changed := map[string]any{}
	if req.AnonymousRead != nil && *req.AnonymousRead != row.AnonymousRead {
		row.AnonymousRead = *req.AnonymousRead
		changed["anonymousRead"] = row.AnonymousRead
	}
	if req.Policy != nil && row.Kind == index.KindGroup {
		return nil, invalid("policy: a group has none; its members' policies apply")
	}
	if req.Policy != nil {
		policy, err := mergePolicy(row.Format, row.Policy, req.Policy)
		if err != nil {
			return nil, err
		}
		if !samePolicy(row.Policy, policy) {
			row.Policy = policy
			changed["policy"] = policy
		}
	}
	return changed, nil
}

// Delete removes the repo formatName/name with one repo.delete audit event. A repo that has
// versions is refused with an error wrapping index.ErrConflict that gives their number, unless
// force is set. Then the index rows that refer to the repo go with it (index.Writer.DeleteRepo:
// packages, versions, files, the index cache, OCI repositories, tag claims, deploy tokens,
// keys and sessions), and the registry is untouched, which the audit details say:
// {"versions": N, "force": true, "registryUntouched": true}. Bindings name repos by mask and
// stay. A repo that does not exist is an error wrapping index.ErrNotFound.
func (s *Repos) Delete(ctx context.Context, formatName, name string, force bool, o Origin) error {
	var ev index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) (err error) {
		row, err := w.GetRepo(ctx, formatName, name)
		if err != nil {
			return err
		}
		if err := notAMember(ctx, w, row); err != nil {
			return err
		}
		n, err := countVersions(ctx, w, row.ID)
		if err != nil {
			return err
		}
		if n > 0 && !force {
			return &clientError{
				msg: fmt.Sprintf("repo %s/%s has %s; delete them or pass force=true", formatName, name, versions(n)),
				err: index.ErrConflict,
			}
		}
		if err = w.DeleteRepo(ctx, row.ID); err != nil {
			return err
		}
		ev, err = s.record(ctx, w, actionDelete, row, o,
			map[string]any{"versions": n, "force": force, "registryUntouched": true})
		return err
	})
	if err != nil {
		return repoError("delete", formatName, name, err)
	}
	s.writeAudit(ctx, ev)
	return nil
}

// countVersions returns the number of versions in a repo. index.Reader has no count, so it
// lists the packages and their versions: one query per package.
func countVersions(ctx context.Context, r index.Reader, repoID int64) (int, error) {
	pkgs, err := r.ListPackages(ctx, repoID, "")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range pkgs {
		vs, err := r.ListVersions(ctx, p.ID)
		if err != nil {
			return 0, err
		}
		n += len(vs)
	}
	return n, nil
}

// versions returns "1 version" or "N versions".
func versions(n int) string {
	if n == 1 {
		return "1 version"
	}
	return fmt.Sprintf("%d versions", n)
}

// Bootstrap merges the repos of the configuration (repos[]) into the control plane at start,
// after the migrations and before the server listens, and returns how many it created. An entry
// that is not in the control plane is created exactly as Create would, by the anonymous
// principal with the source config. An entry that exists is left as it is; when its registry,
// anonymousRead or policy differ from the control plane, a warning names the repo and the
// field: the control plane wins. Bootstrap never deletes and never updates.
//
// The configuration was validated already, and the checks of Create are the same but for the
// format, which must be compiled in, and the policy, which only Create checks; the error of
// an entry is returned, naming the entry, with the repos before it created. A repo that
// another replica creates between the lookup and the create is a conflict, and counts as
// existing.
func (s *Repos) Bootstrap(ctx context.Context, repos []config.Repo) (created int, err error) {
	ctx = auth.WithPrincipal(ctx, auth.Anonymous)
	for _, i := range groupsLast(repos) {
		c := repos[i]
		id := c.Format + "/" + c.Name
		existing, err := s.Index.GetRepo(ctx, c.Format, c.Name)
		switch {
		case err == nil:
			s.warnDrift(ctx, existing, c)
			continue
		case !errors.Is(err, index.ErrNotFound):
			return created, fmt.Errorf("api: bootstrap repos[%d] %s: %w", i, id, err)
		}
		policy, err := configJSON("policy", c.Policy)
		if err != nil {
			return created, fmt.Errorf("repos[%d]: %w", i, err)
		}
		upstream, err := configJSON("upstream", c.Upstream)
		if err != nil {
			return created, fmt.Errorf("repos[%d]: %w", i, err)
		}
		group, err := configJSON("group", c.Group)
		if err != nil {
			return created, fmt.Errorf("repos[%d]: %w", i, err)
		}
		req := CreateRequest{Format: c.Format, Name: c.Name, Registry: c.Registry, Kind: c.Kind, AnonymousRead: c.AnonymousRead,
			Policy: policy, Upstream: upstream, Group: group}
		switch _, err := s.Create(ctx, req, Origin{Source: SourceConfig}); {
		case errors.Is(err, index.ErrConflict):
			s.log().InfoContext(ctx, "repo of the configuration was created meanwhile, by another replica", "repo", id)
		case err != nil:
			return created, fmt.Errorf("repos[%d]: %w", i, err)
		default:
			created++
			s.log().InfoContext(ctx, "repo of the configuration created", "repo", id, "registry", c.Registry)
		}
	}
	return created, nil
}

// configJSON is a map of a configured repo (policy, upstream) as JSON, nil for none. YAML
// decodes a map with keys that are not strings, which JSON cannot hold, into a value Marshal
// refuses.
func configJSON(field string, m map[string]any) (json.RawMessage, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, invalid("%s: %s", field, err.Error())
	}
	return b, nil
}

// warnDrift logs a warning for each field of a configured repo that differs from the repo in
// the control plane. The upstream is compared as ParseSettings reads it, defaults filled in.
func (s *Repos) warnDrift(ctx context.Context, row *index.Repo, c config.Repo) {
	policy, err := configJSON("policy", c.Policy)
	if err != nil || !samePolicy(policy, row.Policy) {
		s.warnField(ctx, row, "policy", string(policy), string(row.Policy))
	}
	upstream, err := configJSON("upstream", c.Upstream)
	if settings, perr := proxy.ParseSettings(upstream); err == nil && perr == nil {
		upstream = settings.JSON()
	}
	if !samePolicy(upstream, row.Upstream) {
		s.warnField(ctx, row, "upstream", string(upstream), string(row.Upstream))
	}
	group, err := configJSON("group", c.Group)
	if g, gerr := proxy.ParseGroup(group); err == nil && gerr == nil {
		group = g.JSON()
	}
	if current, err := resource(ctx, s.Index, row); err == nil && !samePolicy(group, current.Group) {
		s.warnField(ctx, row, "group", string(group), string(current.Group))
	}
	kind := c.Kind
	if kind == "" {
		kind = index.KindHosted
	}
	for _, f := range []struct {
		field         string
		config, plane any
	}{
		{"registry", c.Registry, row.Registry},
		{"kind", kind, row.Kind},
		{"anonymousRead", c.AnonymousRead, row.AnonymousRead},
	} {
		if f.config != f.plane {
			s.warnField(ctx, row, f.field, f.config, f.plane)
		}
	}
}

// warnField logs that field of the configured repo row differs from the control plane.
func (s *Repos) warnField(ctx context.Context, row *index.Repo, field string, configured, plane any) {
	s.log().WarnContext(ctx, "repo of the configuration differs from the control plane, and the control plane wins;"+
		" change it through the API or lajstrom admin repo",
		"repo", row.Format+"/"+row.Name, "field", field, "config", configured, "controlPlane", plane)
}

// newRow validates req and returns the repos row to insert, and a group's settings, whose
// members the caller checks against the index (checkMembers).
func (s *Repos) newRow(req CreateRequest) (index.Repo, *proxy.GroupSettings, error) {
	checks := []error{s.checkFormat(req.Format), checkName(req.Name)}
	if req.Kind != index.KindGroup {
		checks = append(checks, s.checkRegistry(req.Registry))
	}
	for _, err := range checks {
		if err != nil {
			return index.Repo{}, nil, err
		}
	}
	kind, upstream, err := s.checkKind(req)
	if err != nil {
		return index.Repo{}, nil, err
	}
	if kind == index.KindGroup {
		group, err := s.checkGroup(req)
		if err != nil {
			return index.Repo{}, nil, err
		}
		now := s.now()
		return index.Repo{Format: req.Format, Name: req.Name, Prefix: req.Format + "/" + req.Name, Kind: kind,
			AnonymousRead: req.AnonymousRead, Group: group.Stored(), CreatedAt: now, UpdatedAt: now}, &group, nil
	}
	policy, err := checkPolicy(req.Format, req.Policy)
	if err != nil {
		return index.Repo{}, nil, err
	}
	now := s.now()
	return index.Repo{
		Format: req.Format, Name: req.Name, Registry: req.Registry, Prefix: req.Format + "/" + req.Name,
		Kind: kind, AnonymousRead: req.AnonymousRead, Policy: policy, Upstream: upstream, CreatedAt: now, UpdatedAt: now,
	}, nil, nil
}

// checkFormat checks that a format plugin of that name is compiled in, or that it is oci.
func (s *Repos) checkFormat(name string) error {
	registered := s.Formats
	if registered == nil {
		registered = func(n string) bool { _, ok := format.Lookup(n); return ok }
	}
	switch {
	case name == "":
		return invalid("format: must not be empty")
	case name == formatOCI:
		return nil
	case !registered(name):
		return invalid("format: %q is not a format this server has", name)
	}
	return nil
}

// checkName checks a repo name against docs/spec/manifest-profile.md §4.1, the rule the
// configuration applies too.
func checkName(name string) error {
	switch {
	case name == "":
		return invalid("name: must not be empty")
	case len(name) > config.MaxNameLen || !config.RepoNamePattern.MatchString(name):
		return invalid("name: %q must be lowercase letters and digits separated by single '.', '_' or '-', at most %d characters",
			name, config.MaxNameLen)
	case profile.ReservedRepoName(name):
		return invalid("name: %q %s", name, config.ReservedRepoName)
	}
	return nil
}

// checkRegistry checks that a registry of that name is configured.
func (s *Repos) checkRegistry(name string) error {
	switch {
	case name == "":
		return invalid("registry: must not be empty")
	case s.Registries == nil || !s.Registries(name):
		return invalid("registry: %q is not a configured registry", name)
	}
	return nil
}

// checkKind checks the kind of req and the upstream that goes with it (docs/spec/proxy-and-group.md
// §2.5), and returns the kind and the upstream to store: on a proxy the settings with every key
// (proxy.Settings.JSON), else nil. A proxy needs a format whose plugin has proxy repos; group
// repos come with #311.
func (s *Repos) checkKind(req CreateRequest) (string, json.RawMessage, error) {
	kind := req.Kind
	if kind == "" {
		kind = index.KindHosted
	}
	switch {
	case kind != index.KindHosted && kind != index.KindProxy && kind != index.KindGroup:
		return "", nil, invalid("kind: %q is not %s, %s or %s", req.Kind, index.KindHosted, index.KindProxy, index.KindGroup)
	case kind != index.KindProxy && !isNull(req.Upstream):
		return "", nil, invalid("upstream: only a proxy repo has one, not a %s repo", kind)
	case kind != index.KindGroup && !isNull(req.Group):
		return "", nil, invalid("group: only a group repo has one, not a %s repo", kind)
	case kind == index.KindHosted:
		return index.KindHosted, nil, nil
	case kind == index.KindGroup:
		return index.KindGroup, nil, nil
	}
	if _, ok := s.proxier(req.Format); !ok {
		return "", nil, invalid("kind: format %s has no proxy repos", req.Format)
	}
	if isNull(req.Upstream) {
		return "", nil, invalid("upstream: a proxy repo must name one of upstreams[].name of the configuration")
	}
	settings, err := s.checkUpstream(req.Format, req.Upstream)
	if err != nil {
		return "", nil, err
	}
	return index.KindProxy, settings.JSON(), nil
}

// checkUpstream reads the settings of a proxy repo of formatName and checks that they name a
// configured upstream of that format. The API never takes the upstream's URL: the settings have
// no key for it.
func (s *Repos) checkUpstream(formatName string, raw json.RawMessage) (proxy.Settings, error) {
	settings, err := proxy.ParseSettings(raw)
	if err != nil {
		return proxy.Settings{}, &clientError{msg: err.Error(), err: err} // the message names the field
	}
	var upstreamFormat string
	ok := false
	if s.Upstreams != nil {
		upstreamFormat, ok = s.Upstreams(settings.Upstream)
	}
	switch {
	case !ok:
		return proxy.Settings{}, invalid("upstream.name: %q is not a configured upstream", settings.Upstream)
	case upstreamFormat != formatName:
		return proxy.Settings{}, invalid("upstream.name: %q serves %s, not %s", settings.Upstream, upstreamFormat, formatName)
	}
	return settings, nil
}

// proxier returns the proxy hooks of the plugin of formatName, when it has proxy repos.
func (s *Repos) proxier(formatName string) (format.Proxier, bool) {
	if s.Proxiers != nil {
		return s.Proxiers(formatName)
	}
	p, ok := format.Lookup(formatName)
	if !ok {
		return nil, false
	}
	px, ok := p.(format.Proxier)
	return px, ok
}

// isNull reports whether a JSON member is absent or the JSON null.
func isNull(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || string(raw) == "null"
}

// samePolicy reports whether two policies are the same JSON value. Postgres keeps a jsonb
// column as a value, not as the bytes that were written.
func samePolicy(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// record builds the audit event of a change of row, with details and the origin, and appends
// it on w, in the transaction of the change, when there is no audit.Writer. The caller passes
// the event to writeAudit after the commit.
func (s *Repos) record(ctx context.Context, w index.Writer, action string, row *index.Repo, o Origin, details map[string]any) (index.AuditEvent, error) {
	if o.Source != "" {
		details["source"] = o.Source
	}
	if o.RequestID != "" {
		details["requestId"] = o.RequestID
	}
	if o.Host != "" {
		details["host"] = o.Host
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return index.AuditEvent{}, fmt.Errorf("api: audit details of %s: %w", action, err)
	}
	ev := index.AuditEvent{
		At: s.now(), Principal: auth.PrincipalFrom(ctx).String(), Action: action,
		Repo: row.Format + "/" + row.Name, IP: o.IP, Details: raw,
	}
	if s.Audit == nil {
		if err := w.AppendAudit(ctx, &ev); err != nil {
			return index.AuditEvent{}, err
		}
	}
	return ev, nil
}

// writeAudit passes ev to the audit.Writer, after the commit of its change. A failure is
// logged: the change is made, and is not undone for its event.
func (s *Repos) writeAudit(ctx context.Context, ev index.AuditEvent) {
	if s.Audit == nil {
		return
	}
	if err := s.Audit.Write(ctx, ev); err != nil {
		s.log().ErrorContext(ctx, "api: audit event of a repo change not written",
			"action", ev.Action, "repo", ev.Repo, "error", err.Error())
	}
}

// repoError turns an error about the repo formatName/name into the one the callers answer:
// index.ErrNotFound and index.ErrConflict get a message for the client, a *clientError stays as
// it is, anything else is wrapped with op, for the log.
func repoError(op, formatName, name string, err error) error {
	var ce *clientError
	switch {
	case errors.As(err, &ce):
		return err
	case errors.Is(err, index.ErrNotFound):
		return &clientError{msg: fmt.Sprintf("repo %s/%s does not exist", formatName, name), err: err}
	case errors.Is(err, index.ErrConflict):
		return &clientError{msg: fmt.Sprintf("repo %s/%s already exists", formatName, name), err: err}
	}
	return fmt.Errorf("api: %s repo %s/%s: %w", op, formatName, name, err)
}

// resource returns the API form of a repos row, with UTC times; a group's with its members, read
// on rd.
func resource(ctx context.Context, rd index.Reader, r *index.Repo) (Repo, error) {
	out := Repo{
		Format: r.Format, Name: r.Name, Registry: r.Registry, Kind: r.Kind, AnonymousRead: r.AnonymousRead,
		Policy: r.Policy, Upstream: r.Upstream, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
	if r.Kind != index.KindGroup {
		return out, nil
	}
	g, err := groupOf(ctx, rd, r)
	if err != nil {
		return Repo{}, err
	}
	out.Group = g.JSON()
	return out, nil
}

func (s *Repos) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Repos) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}
