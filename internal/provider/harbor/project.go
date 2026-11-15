package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/lajstrom/lajstrom/internal/provider"
)

const (
	// knownFor is how long EnsureRepository believes, without asking Harbor, that the project
	// exists: ingest ensures the repository of every publish. A project deleted behind
	// Lajstrom's back is found gone, or created again, after at most this long.
	knownFor = 10 * time.Minute
	// maxFactsBody bounds the answers that readFacts decodes.
	maxFactsBody = 1 << 20
	// missingHint is the hint of a project that does not exist. It names no project: the
	// project is the instance prefix, which clients never see.
	missingHint = "the Harbor project of registries[].prefix does not exist: create it, or set registries[].harbor.createProject"
	// createHint is the hint of a project that the account may not create.
	createHint = "Harbor lets only a system robot account with project:create, or an administrator, create a project; " +
		"create it, or give the account the permission"
)

// EnsureRepository makes sure that the project exists, which is all a push to a new repository
// needs on Harbor: the repository is created by its first manifest, at any depth below the
// project. The project is asked for once per knownFor. A project that does not exist is
// created with registries[].harbor.createProject (ensureProject), and is otherwise a
// *provider.RejectedError of 404 whose hint names that key: the client of a publish gets 502.
func (p *Provider) EnsureRepository(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("harbor: ensure repository: empty name")
	}
	return p.ensureProject(ctx, "ensure repository "+name)
}

// projectKnown reports whether the project was found or created within knownFor.
func (p *Provider) projectKnown() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.now().Before(p.projectUntil)
}

// ensureProject asks Harbor whether the project exists, HEAD /api/v2.0/projects?project_name=,
// which any account may ask, unless it was found within knownFor; op names the caller's
// operation in an error. A project that does not exist is created with
// registries[].harbor.createProject: POST /api/v2.0/projects, private, without a storage
// limit of its own (Harbor's default quota); a 409, a project created meanwhile, is success.
func (p *Provider) ensureProject(ctx context.Context, op string) error {
	if p.projectKnown() {
		return nil
	}
	resp, err := p.api.do(ctx, http.MethodHead, "/projects", url.Values{"project_name": {p.project}}, nil)
	if err != nil {
		return failed(op, err)
	}
	drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		if !p.createProject {
			if p.firstMiss() {
				p.log.ErrorContext(ctx, "the Harbor project does not exist: every publish to the registry fails; "+
					"create it, or set registries[].harbor.createProject", "project", p.project)
			}
			return &apiError{op: op, kind: &provider.RejectedError{Registry: p.name, Status: http.StatusNotFound, Hint: missingHint}}
		}
		if err := p.create(ctx, op); err != nil {
			return err
		}
	default:
		return p.api.translate(op, resp, "")
	}
	p.mu.Lock()
	p.projectUntil = p.now().Add(knownFor)
	p.mu.Unlock()
	return nil
}

// firstMiss reports whether no miss of the project was logged within knownFor, and makes this
// one the last: a missing project, or one the account may not create, fails every publish, and
// is logged once per knownFor.
func (p *Provider) firstMiss() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !p.missedAt.IsZero() && now.Sub(p.missedAt) < knownFor {
		return false
	}
	p.missedAt = now
	return true
}

// create creates the project: private, and with Harbor's default quota, the quota being the
// Harbor administrator's to change.
func (p *Provider) create(ctx context.Context, op string) error {
	body := map[string]any{"project_name": p.project, "metadata": map[string]string{"public": "false"}}
	resp, err := p.api.do(ctx, http.MethodPost, "/projects", nil, body)
	if err != nil {
		return failed(op, err)
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusCreated:
		p.log.InfoContext(ctx, "Harbor project created", "project", p.project)
	case http.StatusConflict:
	case http.StatusForbidden:
		err := p.api.translate(op, resp, "")
		var e *apiError
		if errors.As(err, &e) {
			e.detail += "; " + createHint
		}
		if p.firstMiss() {
			p.log.ErrorContext(ctx, "the account may not create the Harbor project; every publish to the registry fails", "project", p.project)
		}
		return err
	default:
		return p.api.translate(op, resp, "")
	}
	return nil
}

// readFacts returns the facts of the first call that succeeded, or reads them: the project is
// ensured (ensureProject); unless registries[].capabilities.catalog is set, one entry of the
// project's repositories is asked for, and a 401 or 403 makes catalog false (the account lacks
// repository:list); unless capabilities.immutableTags is set, the project's immutability rules
// are counted (countRules). It logs what it found at info, and warns about what an
// immutability rule refuses Lajstrom.
func (p *Provider) readFacts(ctx context.Context) (facts, error) {
	p.check.Lock()
	defer p.check.Unlock()
	p.mu.Lock()
	f := p.facts
	p.mu.Unlock()
	if f != nil {
		return *f, nil
	}
	if err := p.ensureProject(ctx, "probe the project"); err != nil {
		return facts{}, err
	}
	got := facts{catalog: true, rules: -1}
	if p.override.Catalog == nil {
		ok, err := p.mayList(ctx)
		if err != nil {
			return facts{}, err
		}
		got.catalog = ok
	}
	if p.override.ImmutableTags == nil {
		got.rules = p.countRules(ctx)
	}
	p.log.InfoContext(ctx, "Harbor project", "project", p.project, "listRepositories", got.catalog, "immutabilityRules", got.rules)
	if p.override.Catalog == nil && !got.catalog {
		p.log.WarnContext(ctx, "the account may not list the Harbor project's repositories: a reindex reads the OCI repositories "+
			"the index records (oci_repos); give it repository:list", "project", p.project)
	}
	if got.rules > 0 {
		p.log.WarnContext(ctx, "the Harbor project has immutability rules: Harbor refuses to delete a manifest under a tag they "+
			"match (a version's delete and prune: 409), to move such a tag (the sha256-<digest> index of the referrers tag "+
			"schema, an oci proxy's moved tag), and to delete the repository that holds one", "project", p.project, "rules", got.rules)
	}
	p.mu.Lock()
	p.facts = &got
	p.mu.Unlock()
	return got, nil
}

// mayList asks for one entry of the project's repositories: true for a 200, false for a 401 or
// 403, an error for any other answer.
func (p *Provider) mayList(ctx context.Context) (bool, error) {
	const op = "probe the listing of repositories"
	resp, err := p.api.do(ctx, http.MethodGet, p.repositoriesPath(), url.Values{"page_size": {"1"}}, nil)
	if err != nil {
		return false, failed(op, err)
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, nil
	}
	return false, p.api.translate(op, resp, "repository:list")
}

// rule is the part of an immutability rule of Harbor's REST API that countRules reads.
type rule struct {
	Disabled bool   `json:"disabled"`
	Action   string `json:"action"`
}

// countRules returns the number of the project's immutability rules that are not disabled,
// GET /api/v2.0/projects/<project>/immutabletagrules: a project has at most 15. It returns -1,
// and logs why, when the account may not read them (immutable-tag:list) or the answer does not
// tell: the rules decide nothing that a refusal of Harbor's does not tell as well.
func (p *Provider) countRules(ctx context.Context) int {
	path := "/projects/" + url.PathEscape(p.project) + "/immutabletagrules"
	resp, err := p.api.do(ctx, http.MethodGet, path, url.Values{"page_size": {"100"}}, nil)
	if err != nil {
		p.log.WarnContext(ctx, "could not read the Harbor project's immutability rules", "project", p.project, "error", failed("read the immutability rules", err).Error())
		return -1
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		p.log.InfoContext(ctx, "the account may not read the Harbor project's immutability rules (immutable-tag:list): "+
			"capabilities.immutableTags stays false unless the configuration sets it", "project", p.project)
		return -1
	default:
		p.log.WarnContext(ctx, "could not read the Harbor project's immutability rules", "project", p.project, "status", strconv.Itoa(resp.StatusCode))
		return -1
	}
	var rules []rule
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFactsBody)).Decode(&rules); err != nil {
		p.log.WarnContext(ctx, "could not read the Harbor project's immutability rules", "project", p.project, "error", err.Error())
		return -1
	}
	n := 0
	for _, r := range rules {
		if !r.Disabled && (r.Action == "" || r.Action == "immutable") {
			n++
		}
	}
	return n
}

// repositoriesPath is the path of the project's repositories below apiPath.
func (p *Provider) repositoriesPath() string {
	return "/projects/" + url.PathEscape(p.project) + "/repositories"
}
