package probe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// createStage is when a new repository appeared: with the first upload session, the first blob
// or the first manifest; createNever when the registry refused them.
type createStage int

const (
	createNever createStage = iota
	createSession
	createBlob
	createManifest
)

// measureCreate measures "Create on push" and "Where a repository may be created": in a new
// repository, an upload session, a blob and a manifest, and after each whether the repository
// exists; then a manifest eight levels deep, inside that repository, and names of 255 and 256
// characters.
func (r *run) measureCreate(ctx context.Context) error {
	const repo = "create"
	stage, refused, err := r.createStage(ctx, repo)
	if err != nil {
		return err
	}
	if stage == createNever && r.backend.repos != nil {
		// ECR without a creation template: from here on, each repository is created with the
		// registry's API before its first write, as the provider does.
		r.viaAPI = true
		refused += "; the run creates its repositories with the registry's API"
	}
	r.set("createOnPush", createVerdict(stage, refused))
	if stage == createNever && !r.viaAPI {
		return errors.New("no repository was created")
	}
	ctx = r.in(ctx, "where")
	deep := repo + "/a/b/c/d/e/f/g"
	if err := r.ensureBase(ctx, deep); err != nil {
		return err
	}
	a, err := r.pushManifest(ctx, deep, "v1", r.newManifest("deep", "", nil))
	if err != nil {
		return err
	}
	names, err := r.nameLengths(ctx)
	if err != nil {
		return err
	}
	r.set("where", whereVerdict(r.report.Prefix, a, names))
	return nil
}

// createStage pushes to repo step by step and says when it appeared; refused is the answer
// that refused a step.
func (r *run) createStage(ctx context.Context, repo string) (createStage, string, error) {
	a, err := r.startUpload(ctx, repo)
	if err != nil {
		return createNever, "", err
	}
	if !a.ok(http.StatusAccepted, http.StatusCreated) {
		return createNever, a.refusal() + " to the first upload session", nil
	}
	if loc := a.location(); loc != nil {
		_, _ = r.send(ctx, http.MethodDelete, loc.String()) // the cancel has a cell of its own
	}
	steps := []struct {
		stage createStage
		what  string
		push  func() (answer, error)
	}{
		{createSession, "upload session", func() (answer, error) { return a, nil }},
		{createBlob, "blob", func() (answer, error) {
			_, a, err := r.pushBlob(ctx, repo, []byte("{}"))
			r.based[repo] = err == nil && a.ok(http.StatusCreated)
			return a, err
		}},
		{createManifest, "manifest", func() (answer, error) { return r.pushManifest(ctx, repo, "v1", r.newManifest("create", "", nil)) }},
	}
	for _, s := range steps {
		a, err := s.push()
		if err != nil {
			return createNever, "", err
		}
		if !a.ok(http.StatusAccepted, http.StatusCreated, http.StatusOK) {
			return createNever, a.refusal() + " to the first " + s.what, nil
		}
		exists, err := r.exists(ctx, repo)
		if err != nil || exists {
			return s.stage, "", err
		}
	}
	return createNever, "the repository is not there after its first manifest", nil
}

// exists reports whether repo exists: its tag list has tags, or, when it answers an empty
// list, which distribution on S3 does for any name, the catalog lists it.
func (r *run) exists(ctx context.Context, repo string) (bool, error) {
	tags, a, err := r.listTags(ctx, r.path(repo, "tags/list"))
	if err != nil || a.status != http.StatusOK {
		return false, err
	}
	if len(tags) > 0 || !r.found.catalog {
		return true, nil
	}
	names, _, err := r.catalogBelow(ctx, r.name(repo))
	return slices.Contains(names, r.name(repo)), err
}

// createVerdict is the cell "Create on push".
func createVerdict(stage createStage, refused string) string {
	switch stage {
	case createSession:
		return "yes: the first upload session creates the repository"
	case createBlob:
		return "yes: the first blob creates the repository"
	case createManifest:
		return "yes, with the first manifest"
	case createNever:
	}
	return "no: " + refused
}

// nameLength is what an upload session in a name of some length answered, or why the
// registry's API did not create the repository (apiRefused) before it was asked.
type nameLength struct {
	what       string // "255 characters", "256 characters", "a component of 256 bytes"
	answer     answer
	apiRefused string
}

// taken reports whether the name was taken.
func (n nameLength) taken() bool {
	return n.apiRefused == "" && n.answer.ok(http.StatusAccepted, http.StatusCreated)
}

// refusal says how the name was refused.
func (n nameLength) refusal() string {
	if n.apiRefused != "" {
		return "refused by the registry's API (" + n.apiRefused + ")"
	}
	return n.answer.refusal()
}

// nameLengths opens an upload session, and cancels it, in a name of 255 characters, one of
// 256, and when that is taken, one with a component of 256 bytes. Zot makes each a repository,
// which its collector removes. Where the run creates repositories with the registry's API, it
// creates each first, and one the API refuses is refused.
func (r *run) nameLengths(ctx context.Context) ([]nameLength, error) {
	tries := []struct{ what, name string }{
		{"255 characters", r.longName(255)},
		{"256 characters", r.longName(256)},
		{"a component of 256 bytes", r.name("len/" + strings.Repeat("c", 256))},
	}
	var out []nameLength
	for _, t := range tries {
		n := nameLength{what: t.what}
		if err := r.ensure(ctx, r.below(t.name)); err != nil {
			n.apiRefused = err.Error()
		} else {
			a, err := r.send(ctx, http.MethodPost, "/v2/"+t.name+"/blobs/uploads/")
			if err != nil {
				return nil, err
			}
			if loc := a.location(); loc != nil && a.ok(http.StatusAccepted) {
				_, _ = r.send(ctx, http.MethodDelete, loc.String())
			}
			n.answer = a
		}
		out = append(out, n)
		if len(out) == 2 && !n.taken() {
			break
		}
	}
	return out, nil
}

// longName is a name below the scratch prefix of exactly n characters, its components at most
// 64 characters long.
func (r *run) longName(n int) string {
	name := r.name("len")
	for len(name) < n {
		k := min(64, n-len(name)-1)
		if k < 1 {
			name += "x"
			continue
		}
		name += "/" + strings.Repeat("n", k)
	}
	return name
}

// whereVerdict is the cell "Where a repository may be created": the scope (anywhere, or below
// the instance prefix), the push eight levels deep inside another repository, and the name
// lengths.
func whereVerdict(prefix string, deep answer, names []nameLength) string {
	v := "anywhere"
	if prefix != "" {
		v = fmt.Sprintf("below `%s`; outside it not tried", prefix)
	}
	if deep.ok(http.StatusCreated, http.StatusOK) {
		v += "; at any depth, inside another repository too"
	} else {
		v += "; eight levels deep: " + deep.refusal()
	}
	return v + "; " + lengthVerdict(names)
}

// lengthVerdict says what the name lengths found.
func lengthVerdict(names []nameLength) string {
	taken := func(i int) bool { return i < len(names) && names[i].taken() }
	switch {
	case len(names) < 2:
		return "the name length not measured"
	case !taken(0):
		return "255 characters refused: " + names[0].refusal()
	case !taken(1):
		return "at most 255 characters (256: " + names[1].refusal() + ")"
	case len(names) > 2 && !taken(2):
		return "a name component of more than 255 bytes is " + names[2].refusal()
	}
	return "no length limit met: 256 characters, a component of 256 bytes"
}
