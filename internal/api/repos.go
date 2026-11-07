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
)

// Repo is the repos resource of the management API, as GET /api/v1/repos/{format}/{repo}
// answers it (docs/api.md). Format and Name identify the repo: <format>/<name> is the RBAC
// resource and the OCI path below the registry's instance prefix (ADR 0004). The repo's prefix
// in the index is internal, <format>/<name> in the MVP, and not shown.
type Repo struct {
	Format        string          `json:"format"`
	Name          string          `json:"name"`
	Registry      string          `json:"registry"`
	Kind          string          `json:"kind"` // hosted; proxy and group after the MVP
	AnonymousRead bool            `json:"anonymousRead"`
	Policy        json.RawMessage `json:"policy"` // a JSON object, or nil (null)
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

// RepoList is the answer of GET /api/v1/repos and of lajstrom admin repo list --json.
type RepoList struct {
	Repos []Repo `json:"repos"`
}

// CreateRequest is a repo to create: the body of POST /api/v1/repos, and what lajstrom admin
// repo create and the config bootstrap pass to Repos.Create. Kind is empty or hosted. Policy is
// a JSON object, or empty or the JSON null for none; its keys come with the formats.
type CreateRequest struct {
	Format        string          `json:"format"`
	Name          string          `json:"name"`
	Registry      string          `json:"registry"`
	Kind          string          `json:"kind"`
	AnonymousRead bool            `json:"anonymousRead"`
	Policy        json.RawMessage `json:"policy"`
}

// UpdateRequest is a change to a repo, the body of PATCH /api/v1/repos/{format}/{repo}. A nil
// AnonymousRead is left as it is. Policy tells absent from null by its bytes, as
// encoding/json leaves them: nil (absent) leaves the policy, the JSON null clears it, an object
// replaces it. Format, name, registry and kind cannot change: moving a repo to another
// registry would orphan its artifacts.
type UpdateRequest struct {
	AnonymousRead *bool           `json:"anonymousRead"`
	Policy        json.RawMessage `json:"policy"`
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
// it is auth.PrincipalFrom(ctx), anonymous until authentication comes with #45.
type Origin struct {
	// Source is SourceAPI, SourceCLI or SourceConfig; the audit details carry it as "source".
	Source string
	// IP is the client address without the port; empty outside HTTP.
	IP string
	// RequestID is the id of the HTTP request (server.RequestID); the audit details carry it as
	// "requestId" when it is set.
	RequestID string
}

// Audit actions of the repo changes (docs/spec/index-schema.md, audit).
const (
	actionCreate = "repo.create"
	actionUpdate = "repo.update"
	actionDelete = "repo.delete"
)

// kindHosted is the only repo kind of this version.
const kindHosted = "hosted"

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
// field: format a compiled-in plugin, name the grammar of docs/spec/manifest-profile.md §4.1
// (at most 64 bytes), registry a configured one, kind empty or hosted, policy a JSON object or
// null. A repo that exists already is an error wrapping index.ErrConflict.
func (s *Repos) Create(ctx context.Context, req CreateRequest, o Origin) (*Repo, error) {
	row, err := s.newRow(req)
	if err != nil {
		return nil, err
	}
	var ev index.AuditEvent
	err = s.Index.Tx(ctx, func(w index.Writer) (err error) {
		if err = w.CreateRepo(ctx, &row); err != nil {
			return err
		}
		ev, err = s.record(ctx, w, actionCreate, &row, o, map[string]any{"repo": resource(&row)})
		return err
	})
	if err != nil {
		return nil, repoError("create", req.Format, req.Name, err)
	}
	s.writeAudit(ctx, ev)
	r := resource(&row)
	return &r, nil
}

// Get returns the repo formatName/name, or an error wrapping index.ErrNotFound.
func (s *Repos) Get(ctx context.Context, formatName, name string) (*Repo, error) {
	row, err := s.Index.GetRepo(ctx, formatName, name)
	if err != nil {
		return nil, repoError("get", formatName, name, err)
	}
	r := resource(row)
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
		repos[i] = resource(&rows[i])
	}
	return repos, nil
}

// Update applies req to the repo formatName/name in one transaction with one repo.update audit
// event, whose details name the fields that changed with their new values. A request that
// changes nothing writes nothing and returns the repo as it is. Errors wrap format.ErrInvalid
// (a policy that is neither an object nor null) or index.ErrNotFound.
func (s *Repos) Update(ctx context.Context, formatName, name string, req UpdateRequest, o Origin) (*Repo, error) {
	policy, err := checkPolicy(req.Policy)
	if err != nil {
		return nil, err
	}
	var (
		row *index.Repo
		ev  *index.AuditEvent
	)
	err = s.Index.Tx(ctx, func(w index.Writer) (err error) {
		if row, err = w.GetRepo(ctx, formatName, name); err != nil {
			return err
		}
		changed := applyUpdate(row, req, policy)
		if len(changed) == 0 {
			return nil
		}
		if err = w.UpdateRepo(ctx, row); err != nil {
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
	r := resource(row)
	return &r, nil
}

// applyUpdate changes row as req says and returns the fields that changed, with their new
// values. policy is req.Policy as checkPolicy returned it.
func applyUpdate(row *index.Repo, req UpdateRequest, policy json.RawMessage) map[string]any {
	changed := map[string]any{}
	if req.AnonymousRead != nil && *req.AnonymousRead != row.AnonymousRead {
		row.AnonymousRead = *req.AnonymousRead
		changed["anonymousRead"] = row.AnonymousRead
	}
	if req.Policy != nil && !samePolicy(row.Policy, policy) {
		row.Policy = policy
		changed["policy"] = policy
	}
	return changed
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
// principal with the source config. An entry that exists is left as it is; when its registry
// or anonymousRead differ from the control plane, a warning names the repo and the field: the
// control plane wins. Bootstrap never deletes and never updates.
//
// The configuration was validated already, and the checks of Create are the same, so the
// error of an entry is for a format that is not compiled in; it is returned, naming the entry,
// with the repos before it created. A repo that another replica creates between the lookup
// and the create is a conflict, and counts as existing.
func (s *Repos) Bootstrap(ctx context.Context, repos []config.Repo) (created int, err error) {
	ctx = auth.WithPrincipal(ctx, auth.Anonymous)
	for i, c := range repos {
		id := c.Format + "/" + c.Name
		existing, err := s.Index.GetRepo(ctx, c.Format, c.Name)
		switch {
		case err == nil:
			s.warnDrift(ctx, existing, c)
			continue
		case !errors.Is(err, index.ErrNotFound):
			return created, fmt.Errorf("api: bootstrap repos[%d] %s: %w", i, id, err)
		}
		req := CreateRequest{Format: c.Format, Name: c.Name, Registry: c.Registry, AnonymousRead: c.AnonymousRead}
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

// warnDrift logs a warning for each field of a configured repo that differs from the repo in
// the control plane.
func (s *Repos) warnDrift(ctx context.Context, row *index.Repo, c config.Repo) {
	for _, f := range []struct {
		field         string
		config, plane any
	}{
		{"registry", c.Registry, row.Registry},
		{"anonymousRead", c.AnonymousRead, row.AnonymousRead},
	} {
		if f.config != f.plane {
			s.log().WarnContext(ctx, "repo of the configuration differs from the control plane, and the control plane wins;"+
				" change it through the API or lajstrom admin repo",
				"repo", row.Format+"/"+row.Name, "field", f.field, "config", f.config, "controlPlane", f.plane)
		}
	}
}

// newRow validates req and returns the repos row to insert.
func (s *Repos) newRow(req CreateRequest) (index.Repo, error) {
	for _, err := range []error{s.checkFormat(req.Format), checkName(req.Name), s.checkRegistry(req.Registry), checkKind(req.Kind)} {
		if err != nil {
			return index.Repo{}, err
		}
	}
	policy, err := checkPolicy(req.Policy)
	if err != nil {
		return index.Repo{}, err
	}
	now := s.now()
	return index.Repo{
		Format: req.Format, Name: req.Name, Registry: req.Registry, Prefix: req.Format + "/" + req.Name,
		Kind: kindHosted, AnonymousRead: req.AnonymousRead, Policy: policy, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// checkFormat checks that a format plugin of that name is compiled in.
func (s *Repos) checkFormat(name string) error {
	registered := s.Formats
	if registered == nil {
		registered = func(n string) bool { _, ok := format.Lookup(n); return ok }
	}
	switch {
	case name == "":
		return invalid("format: must not be empty")
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

// checkKind checks that kind is empty or hosted.
func checkKind(kind string) error {
	if kind != "" && kind != kindHosted {
		return invalid("kind: %q is not supported; every repo is %s in this version", kind, kindHosted)
	}
	return nil
}

// checkPolicy returns p compacted, or nil when p is empty or the JSON null. A policy is an
// opaque JSON object whose keys come with the formats (maxFileSize, maven.sessionTimeout); any
// other JSON value is refused.
func checkPolicy(p json.RawMessage) (json.RawMessage, error) {
	p = bytes.TrimSpace(p)
	if len(p) == 0 || string(p) == "null" {
		return nil, nil
	}
	var buf bytes.Buffer
	if p[0] != '{' || json.Compact(&buf, p) != nil { // Compact fails on anything but one JSON value
		return nil, invalid("policy: must be a JSON object or null")
	}
	return buf.Bytes(), nil
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

// resource returns the API form of a repos row, with UTC times.
func resource(r *index.Repo) Repo {
	return Repo{
		Format: r.Format, Name: r.Name, Registry: r.Registry, Kind: r.Kind, AnonymousRead: r.AnonymousRead,
		Policy: r.Policy, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
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
