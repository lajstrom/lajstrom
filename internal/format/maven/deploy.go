package maven

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// maxMetadataXML bounds the maven-metadata.xml a client PUTs: it lists the versions of one
// artifact, a few dozen bytes each.
const maxMetadataXML = 1 << 20

// maxChecksumFile bounds a checksum file: the hex digest, maybe followed by the file name.
const maxChecksumFile = 1 << 10

// deployer serves the deploy path of the maven frontend over one format.Deps.
type deployer struct {
	deps format.Deps
}

// deployRoutes mounts the deploy path (docs/spec/http-routing.md §3, maven): mvn deploy and
// Gradle's maven-publish PUT every file of a version, each followed by its checksum files, and
// then the artifact's maven-metadata.xml, which commits the version. The files stream into the
// registry and the publication session of the version (format.Sessions) as they arrive (ADR
// 0014), with nothing on a local disk; nothing is published before the commit, and a session
// that is not committed expires with its files.
func deployRoutes(r format.Router, deps format.Deps) {
	d := &deployer{deps: deps}
	r.HandleFunc("PUT /{repo}/{path...}", d.put)
}

// put answers PUT /maven/{repo}/{path...}, by what the path names: a file of a version is
// added to its session (201), a checksum file is checked against the file it names (201, or
// 400 when it differs), a maven-metadata.xml commits the sessions of the versions it lists
// (201 for a new version, 200 for the same files again, 409 for other files under a version
// that exists). It needs write on the repo.
func (d *deployer) put(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repo, ok := format.RepoFrom(ctx)
	if !ok {
		d.fail(w, r, errors.New("maven: the route has no repo"))
		return
	}
	if err := format.Authorize(ctx, d.deps, auth.ActionWrite, repo); err != nil {
		d.fail(w, r, err)
		return
	}
	if d.deps.Sessions == nil {
		d.fail(w, r, errors.New("maven: no publication sessions in format.Deps"))
		return
	}
	body, err := format.LimitBody(w, r, repo)
	if err != nil {
		d.fail(w, r, err)
		return
	}
	path := r.PathValue("path")
	var status int
	switch Classify(path) {
	case KindChecksum:
		status, err = d.checksum(r, repo, path, body)
	case KindMetadata:
		status, err = d.metadata(r, repo, path, body)
	case KindFile:
		status, err = d.file(r, repo, path, body)
	}
	if err != nil {
		d.fail(w, r, err)
		return
	}
	w.WriteHeader(status)
}

// file adds the file at path to the session of its version, opened for the request's principal
// when there is none: the body streams into the registry while the session computes its
// checksums (Sessions.Add).
func (d *deployer) file(r *http.Request, repo index.Repo, path string, body io.Reader) (int, error) {
	ctx := r.Context()
	f, err := ParsePath(path)
	if err != nil {
		return 0, err
	}
	if err := d.checkSpelling(r, repo, f.Coordinates); err != nil {
		return 0, err
	}
	s, err := d.deps.Sessions.Open(ctx, repo, f.Name(), f.Version, auth.PrincipalFrom(ctx))
	if err != nil {
		return 0, err
	}
	if _, err := d.deps.Sessions.Add(ctx, s.ID, path, body, r.ContentLength); err != nil {
		return 0, err
	}
	return http.StatusCreated, nil
}

// checkSpelling refuses a file of c when another spelling of its groupId:artifactId holds the
// package path (docs/spec/manifest-profile.md §4.1): the package path is lowercase, so
// Com.Example:lib and com.example:lib are one OCI repository, and the first spelling published
// keeps it. A package of another name there is index.ErrConflict naming it (409). The check runs
// before the session is opened, so that a refused file is not stored. Two spellings deployed at
// once both pass it; ingest settles that race under its claims before either manifest is pushed
// (#244).
func (d *deployer) checkSpelling(r *http.Request, repo index.Repo, c Coordinates) error {
	pkg, err := index.PackageAt(r.Context(), d.deps.Index, repo.ID, packagePath(c.GroupID, c.ArtifactID))
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("maven: package path of %s: %w", c.Name(), err)
	case pkg.Name != c.Name():
		return fmt.Errorf("%w: %s exists as %s: a groupId:artifactId is one package path without regard to case, "+
			"and the first spelling published holds it", index.ErrConflict, c.Name(), pkg.Name)
	}
	return nil
}

// checksum checks the checksum file at path against the file it names in the principal's
// session of that version: the checksums were computed while the file streamed in. A checksum
// file is never stored: the checksums Lajstrom serves are its own. One that differs is 400, and
// the session refuses the file (Sessions.Refuse): mvn logs the 400 of a checksum and goes on to
// maven-metadata.xml, whose commit is then refused too, until the file is uploaded again. The
// checksum of a maven-metadata.xml is accepted unchecked, for the metadata Lajstrom serves is
// rendered from the index, not the one the client sent.
func (d *deployer) checksum(r *http.Request, repo index.Repo, path string, body io.Reader) (int, error) {
	target, algorithm, _ := ChecksumOf(path)
	got, err := readChecksum(body, algorithm, path)
	if err != nil {
		return 0, err
	}
	if Classify(target) == KindMetadata {
		return http.StatusCreated, nil
	}
	f, err := ParsePath(target)
	if err != nil {
		return 0, err
	}
	s, ok, err := d.session(r, repo, f.Coordinates)
	if err != nil {
		return 0, err
	}
	var files []format.SessionFile
	if ok {
		if files, err = d.deps.Sessions.Files(r.Context(), s.ID); err != nil {
			return 0, err
		}
	}
	for _, sf := range files {
		if sf.Name != target {
			continue
		}
		if want := sf.Checksums[algorithm]; got != want {
			reason := fmt.Sprintf("the %s of %s is %s, %s says %s", algorithm, target, want, path, got)
			// A session that expired meanwhile publishes nothing anyway.
			if err := d.deps.Sessions.Refuse(r.Context(), s.ID, target, reason); err != nil && !errors.Is(err, index.ErrNotFound) {
				return 0, fmt.Errorf("maven: refuse %s: %w", target, err)
			}
			return 0, fmt.Errorf("%w: %s; the deploy does not publish until %s is uploaded again", format.ErrInvalid, reason, target)
		}
		return http.StatusCreated, nil
	}
	return 0, fmt.Errorf("%w: %s is the checksum of %s, which this deploy has not uploaded: upload a file before its checksums",
		format.ErrInvalid, path, target)
}

// session returns the request principal's open session of the version c, and false when there
// is none: Open is not called, for a checksum or a maven-metadata.xml does not start a deploy.
// The key is <groupId>:<artifactId>:<version> (docs/spec/index-schema.md, sessions).
func (d *deployer) session(r *http.Request, repo index.Repo, c Coordinates) (*index.Session, bool, error) {
	s, err := d.deps.Index.GetSession(r.Context(), repo.ID, c.Name()+":"+c.Version)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("maven: session of %s: %w", c, err)
	}
	return s, s.Principal == auth.PrincipalFrom(r.Context()).String(), nil
}

// metadata answers the PUT of an artifact's maven-metadata.xml: it commits the request
// principal's session of the version the document was written for, its release (else its
// latest), which mvn and Gradle set to the version they deploy. Only that one: the document
// lists every version of the artifact, and committing another's session would publish the
// files of a deploy that never finished. Without that session it is the repeat of a deploy
// that committed already when the version is in the index (200), and a client error otherwise.
// The metadata of a group, which lists plugin prefixes and no versions, is accepted and not
// kept: the plugin prefixes are after the MVP.
func (d *deployer) metadata(r *http.Request, repo index.Repo, path string, body io.Reader) (int, error) {
	ctx := r.Context()
	c, err := ParseMetadataPath(path)
	if err != nil {
		return 0, err
	}
	md, err := readMetadataXML(body, path)
	if err != nil {
		return 0, err
	}
	if md.Plugins != nil && len(md.versions()) == 0 {
		d.deps.Log.DebugContext(ctx, "maven: the metadata of a group is not kept", "repo", repo.Format+"/"+repo.Name, "path", path)
		return http.StatusCreated, nil
	}
	if (md.GroupID != "" && md.GroupID != c.GroupID) || (md.ArtifactID != "" && md.ArtifactID != c.ArtifactID) {
		return 0, fmt.Errorf("%w: %s is the metadata of %s:%s, its path says %s", format.ErrInvalid, path, md.GroupID, md.ArtifactID, c.Name())
	}
	c.Version = md.current()
	if c.Version == "" {
		return 0, fmt.Errorf("%w: %s names no version", format.ErrInvalid, path)
	}
	pub, err := d.commit(r, repo, c)
	switch {
	case err != nil:
		return 0, err
	case pub == nil:
		return d.repeat(r, repo, c)
	case pub.Existed:
		return http.StatusOK, nil
	}
	return http.StatusCreated, nil
}

// commit commits the request principal's session of the version c, and returns nil without
// one, also when it expired meanwhile. A session whose files conflict with the version in the
// index is aborted: tags are immutable, it would never commit, and a later deploy of the version
// starts empty.
func (d *deployer) commit(r *http.Request, repo index.Repo, c Coordinates) (*format.Published, error) {
	ctx := r.Context()
	s, ok, err := d.session(r, repo, c)
	if err != nil || !ok {
		return nil, err
	}
	pub, err := d.deps.Sessions.Commit(ctx, s.ID)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil, nil
	case errors.Is(err, index.ErrConflict):
		if aerr := d.deps.Sessions.Abort(ctx, s.ID); aerr != nil {
			d.deps.Log.DebugContext(ctx, "maven: the session of a conflict stays", "session", s.ID, "error", aerr.Error())
		}
		return nil, fmt.Errorf("maven: commit %s: %w", c, err)
	case err != nil:
		return nil, fmt.Errorf("maven: commit %s: %w", c, err)
	}
	d.deps.Log.InfoContext(r.Context(), "maven: version deployed", "repo", repo.Format+"/"+repo.Name, "version", c.String(),
		"digest", pub.Digest.String(), "existed", pub.Existed)
	return pub, nil
}

// repeat answers a maven-metadata.xml whose version c has no session to commit: 200 when the
// version is in the index already, the PUT of a deploy that was committed; 400 otherwise.
func (d *deployer) repeat(r *http.Request, repo index.Repo, c Coordinates) (int, error) {
	ctx := r.Context()
	pkg, err := d.deps.Index.GetPackage(ctx, repo.ID, c.Name())
	if err == nil {
		_, err = d.deps.Index.GetVersion(ctx, pkg.ID, c.Version, "")
	}
	switch {
	case err == nil:
		return http.StatusOK, nil
	case !errors.Is(err, index.ErrNotFound):
		return 0, fmt.Errorf("maven: version %s: %w", c, err)
	}
	return 0, fmt.Errorf("%w: this deploy uploaded no files of %s, the version its %s is for; upload the files of a version, then its %s",
		format.ErrInvalid, c, MetadataFile, MetadataFile)
}

// metadataXML is what the deploy path reads of a maven-metadata.xml.
type metadataXML struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Versioning struct {
		Latest   string   `xml:"latest"`
		Release  string   `xml:"release"`
		Versions []string `xml:"versions>version"`
	} `xml:"versioning"`
	Plugins *struct{} `xml:"plugins"`
}

// versions are the versions the document names, each once: versions, release, latest, version.
func (m *metadataXML) versions() []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range append(append([]string{}, m.Versioning.Versions...), m.Versioning.Release, m.Versioning.Latest, m.Version) {
		if v = strings.TrimSpace(v); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// current is the version a deploy wrote the document for: its release, else its latest, else
// its version, else the last of its versions.
func (m *metadataXML) current() string {
	for _, v := range []string{m.Versioning.Release, m.Versioning.Latest, m.Version} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	if n := len(m.Versioning.Versions); n > 0 {
		return strings.TrimSpace(m.Versioning.Versions[n-1])
	}
	return ""
}

// readMetadataXML reads a maven-metadata.xml of at most maxMetadataXML bytes. Errors wrap
// format.ErrInvalid, but for a failure to read the body.
func readMetadataXML(body io.Reader, path string) (*metadataXML, error) {
	b, err := io.ReadAll(io.LimitReader(body, maxMetadataXML+1))
	if err != nil {
		return nil, fmt.Errorf("maven: read %s: %w", path, err)
	}
	if len(b) > maxMetadataXML {
		return nil, fmt.Errorf("%w: %s is over %d bytes", format.ErrInvalid, path, maxMetadataXML)
	}
	var md metadataXML
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.CharsetReader = charsetReader
	if err := dec.Decode(&md); err != nil {
		return nil, fmt.Errorf("%w: %s is not a maven-metadata.xml: %w", format.ErrInvalid, path, err)
	}
	return &md, nil
}

// readChecksum reads a checksum file: the hexadecimal digest of algorithm, maybe followed by
// white space and a file name, as some clients write it. Errors wrap format.ErrInvalid, but for
// a failure to read the body.
func readChecksum(body io.Reader, algorithm, path string) (string, error) {
	b, err := io.ReadAll(io.LimitReader(body, maxChecksumFile+1))
	if err != nil {
		return "", fmt.Errorf("maven: read %s: %w", path, err)
	}
	fields := strings.Fields(string(b))
	if len(b) > maxChecksumFile || len(fields) == 0 {
		return "", fmt.Errorf("%w: %s is not a checksum file: want the %s in hexadecimal", format.ErrInvalid, path, algorithm)
	}
	got := strings.ToLower(fields[0])
	if !isHex(got, hexLen(algorithm)) {
		return "", fmt.Errorf("%w: %s holds %q, not a %s in hexadecimal", format.ErrInvalid, path, fields[0], algorithm)
	}
	return got, nil
}

// hexLen is the number of hex digits of a digest of algorithm, 0 for one that is not of
// algorithms.
func hexLen(algorithm string) int {
	for _, a := range algorithms {
		if a.name == algorithm {
			return a.digits
		}
	}
	return 0
}

// fail answers err with format.WriteError and logs a 5xx, whose cause the client does not see.
func (d *deployer) fail(w http.ResponseWriter, r *http.Request, err error) {
	if status := format.WriteError(w, err); status >= http.StatusInternalServerError {
		d.deps.Log.ErrorContext(r.Context(), "maven: request failed", "method", r.Method, "path", r.URL.EscapedPath(),
			"status", status, "error", err.Error())
	}
}
