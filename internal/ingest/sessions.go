package ingest

import (
	"context"
	"crypto/md5"  //nolint:gosec // maven's .md5 of a file; not a security use
	"crypto/sha1" //nolint:gosec // maven's .sha1 of a file; not a security use
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Publication sessions (format.Sessions; docs/architecture.md, flow B). A session is a row of
// sessions: its key is <name>:<version> (maven g:a:v), its files column a sessionState. Every
// file is pushed to the registry by Add, into the OCI repository the version will be in, and
// recorded with its checksums; Commit hands the recorded files to Upload, whose Build reads what
// it needs back from the registry and whose pushBlobs finds the files there. Nothing of a
// session is ever published but by Commit: a session that expires is dropped, its blobs left to
// the registry's garbage collection.
//
// Every change of a session's files is a transaction that starts with TouchSession, which
// takes the row's lock on Postgres, so that two Adds at once (Gradle uploads in parallel) both
// land; the unique index of open sessions per key (0007) makes two Opens at once end in one
// session. Commit marks the session committing in such a transaction before it publishes, so
// that one Commit runs at a time across replicas; a mark older than ClaimTTL is a Commit that
// died, and the next one takes over. Commit is safe to repeat: the second Upload of the same
// files is the idempotent repeat.

// The states of a session (docs/spec/index-schema.md, sessions).
const (
	sessionOpen      = "open"
	sessionCommitted = "committed"
	sessionAborted   = "aborted"
	sessionExpired   = "expired"
)

// defaultSessionTimeout is Options.SessionTimeout when it is zero: how long a session lives after
// its last request.
const defaultSessionTimeout = 30 * time.Minute

// sessionWriteTimeout bounds the write of a session's row after its commit, which happens even
// when the client has gone.
const sessionWriteTimeout = 10 * time.Second

// sessionBlobType is the media type of a session's blobs as they are pushed; the manifest's
// layers give them theirs.
const sessionBlobType = "application/octet-stream"

// defaultMaxSessionFiles is how many files a publication session holds when the repo's
// maven.maxSessionFiles does not say (U-5 of the threat model, #246): every Add writes the whole
// list again. The largest version on Maven Central, org.nd4j:libnd4j 1.0.0-M1, stores 76 files,
// its signatures counted and its checksums not.
const defaultMaxSessionFiles = 256

var _ format.Sessions = (*Pipeline)(nil)

// sessionState is the files column of a publication session's row.
type sessionState struct {
	// Name and Version are what the session publishes.
	Name    string `json:"name"`
	Version string `json:"version"`
	// Files are the files added, in the order they were first added.
	Files []format.SessionFile `json:"files"`
	// Committing is when a Commit started, while it runs; zero otherwise.
	Committing time.Time `json:"committing,omitzero"`
}

// room refuses a new file name in a session of repo that holds the files its policy allows
// (maxFilesOf); a file it holds may come again, and replaces itself. What the session stores is
// what counts: the maven frontend checks a checksum file against its file and drops it, and
// keeps no maven-metadata.xml, so neither reaches a session.
func (st *sessionState) room(repo index.Repo, name string) error {
	limit := maxFilesOf(repo)
	if len(st.Files) < limit {
		return nil
	}
	for _, f := range st.Files {
		if f.Name == name {
			return nil
		}
	}
	return fmt.Errorf("%w: the deploy of %s %s holds %d files, the bound policy.maven.maxSessionFiles of %s/%s (%d by default; "+
		"checksum files and maven-metadata.xml do not count): %s is one too many; a manager of the repo may raise the bound",
		ErrInvalid, st.Name, st.Version, limit, repo.Format, repo.Name, defaultMaxSessionFiles, name)
}

// sessionKey is the key of the session of name at version.
func sessionKey(name, version string) string { return name + ":" + version }

// Open implements format.Sessions. An open session whose time is up is expired first, so that
// the next one starts empty.
func (p *Pipeline) Open(ctx context.Context, repo index.Repo, name, version string, principal auth.Principal) (*index.Session, error) {
	plugin, _, err := p.lookup(repo)
	if err != nil {
		return nil, err
	}
	if _, err := plugin.EscapeName(name); err != nil {
		return nil, invalid(fmt.Errorf("ingest: session of %s: %w", name, err))
	}
	if version == "" {
		return nil, fmt.Errorf("%w: ingest: session of %s: the version is empty", ErrInvalid, name)
	}
	key := sessionKey(name, version)
	for range 2 { // the second round reads the session that won a race to be opened
		s, err := p.openSession(ctx, repo, key)
		switch {
		case err == nil && s.Principal != principal.String():
			return nil, fmt.Errorf("%w: the publication session of %s %s is open by another principal: it publishes when that one commits, or expires",
				ErrConflict, name, version)
		case err == nil:
			return s, nil
		case !errors.Is(err, index.ErrNotFound):
			return nil, fmt.Errorf("ingest: session %s in %s/%s: %w", key, repo.Format, repo.Name, err)
		}
		state, err := json.Marshal(sessionState{Name: name, Version: version, Files: []format.SessionFile{}})
		if err != nil {
			return nil, fmt.Errorf("ingest: session %s: %w", key, err)
		}
		s = &index.Session{RepoID: repo.ID, Key: key, Principal: principal.String(), State: sessionOpen,
			Files: state, ExpiresAt: p.now().Add(p.timeoutOf(repo))}
		err = p.index.UpsertSession(ctx, s)
		switch {
		case err == nil:
			p.log.DebugContext(ctx, "ingest: session opened", "repo", repo.Format+"/"+repo.Name, "session", s.ID,
				"key", key, "principal", s.Principal)
			return s, nil
		case !errors.Is(err, index.ErrConflict):
			return nil, fmt.Errorf("ingest: open session %s in %s/%s: %w", key, repo.Format, repo.Name, err)
		}
	}
	return nil, fmt.Errorf("ingest: open session %s in %s/%s: it was opened and closed again meanwhile; try again", key, repo.Format, repo.Name)
}

// openSession returns the open session of key in repo, expiring it, and answering
// index.ErrNotFound, when its time is up.
func (p *Pipeline) openSession(ctx context.Context, repo index.Repo, key string) (*index.Session, error) {
	s, err := p.index.GetSession(ctx, repo.ID, key)
	if err != nil {
		return nil, err
	}
	if p.now().Before(s.ExpiresAt) {
		return s, nil
	}
	s.State = sessionExpired
	if err := p.index.UpsertSession(ctx, s); err != nil && !errors.Is(err, index.ErrNotFound) {
		return nil, err
	}
	return nil, fmt.Errorf("%w: session %d expired", index.ErrNotFound, s.ID)
}

// Add implements format.Sessions: the bytes are spooled and hashed, pushed unless the registry
// has them, and recorded.
func (p *Pipeline) Add(ctx context.Context, sessionID int64, name string, r io.Reader, size int64) (format.SessionFile, error) {
	if name == "" {
		return format.SessionFile{}, fmt.Errorf("%w: ingest: a file of session %d has no name", ErrInvalid, sessionID)
	}
	s, st, err := p.liveSession(ctx, sessionID)
	if err != nil {
		return format.SessionFile{}, err
	}
	t, err := p.sessionTarget(ctx, s, st)
	if err != nil {
		return format.SessionFile{}, err
	}
	if err := st.room(t.repo, name); err != nil { // before the bytes are stored; again below, under the lock
		return format.SessionFile{}, err
	}
	f, sf, err := spoolHashed(r, name, size)
	if err != nil {
		return format.SessionFile{}, err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err := t.prov.EnsureRepository(ctx, t.name); err != nil {
		return format.SessionFile{}, fmt.Errorf("ingest: ensure repository %s: %w", t.name, err)
	}
	if err := t.pushBlob(ctx, format.BlobSource{
		Descriptor: ocispec.Descriptor{MediaType: sessionBlobType, Digest: sf.Digest, Size: sf.Size},
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(f, 0, sf.Size)), nil
		},
	}); err != nil {
		return format.SessionFile{}, err
	}
	err = p.changeSession(ctx, sessionID, p.timeoutOf(t.repo), func(st *sessionState) error {
		if !st.Committing.IsZero() {
			return fmt.Errorf("%w: session %d is being committed; %s comes too late for it", ErrConflict, sessionID, name)
		}
		for i := range st.Files {
			if st.Files[i].Name == name {
				st.Files[i] = sf
				return nil
			}
		}
		if err := st.room(t.repo, name); err != nil {
			return err
		}
		st.Files = append(st.Files, sf)
		return nil
	})
	if err != nil {
		return format.SessionFile{}, err
	}
	return sf, nil
}

// Files implements format.Sessions.
func (p *Pipeline) Files(ctx context.Context, sessionID int64) ([]format.SessionFile, error) {
	s, err := p.index.GetSessionByID(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("ingest: session %d: %w", sessionID, err)
	}
	st, err := decodeSession(s)
	if err != nil {
		return nil, err
	}
	return st.Files, nil
}

// Commit implements format.Sessions: it marks the session committing, publishes its files with
// Upload, and closes it. When Upload fails the mark is taken off and the session stays open, so
// that the client may commit again or add what was missing.
func (p *Pipeline) Commit(ctx context.Context, sessionID int64) (*format.Published, error) {
	s, st, err := p.liveSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if len(st.Files) == 0 {
		return nil, fmt.Errorf("%w: session %d of %s %s has no files to commit", ErrInvalid, sessionID, st.Name, st.Version)
	}
	repo, err := p.index.GetRepoByID(ctx, s.RepoID)
	if err != nil {
		return nil, fmt.Errorf("ingest: repo of session %d: %w", s.ID, err)
	}
	timeout := p.timeoutOf(*repo)
	err = p.changeSession(ctx, sessionID, timeout, func(cur *sessionState) error {
		if !cur.Committing.IsZero() && p.now().Sub(cur.Committing) < p.claimTTL {
			return fmt.Errorf("%w: session %d of %s %s is being committed already", ErrConflict, sessionID, st.Name, st.Version)
		}
		cur.Committing = p.now()
		*st = *cur
		return nil
	})
	if err != nil {
		return nil, err
	}
	pub, err := p.commitFiles(ctx, *repo, s, st)
	// The row is written after a client that went away too: a mark left behind would make the
	// next Commit a conflict until it is stale.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionWriteTimeout)
	defer cancel()
	if err != nil {
		if uerr := p.changeSession(wctx, sessionID, timeout, func(cur *sessionState) error { cur.Committing = time.Time{}; return nil }); uerr != nil {
			p.log.WarnContext(ctx, "ingest: session left marked committing; the next commit takes over after the claim TTL",
				"session", sessionID, "error", uerr.Error())
		}
		return nil, err
	}
	s.State = sessionCommitted
	if err := p.index.UpsertSession(wctx, s); err != nil {
		// The version is published; the session expires in its time.
		p.log.ErrorContext(ctx, "ingest: session committed, but its row was not closed", "session", sessionID, "error", err.Error())
	}
	p.log.InfoContext(ctx, "ingest: session committed", "session", sessionID, "key", s.Key, "files", len(st.Files),
		"digest", pub.Digest.String(), "existed", pub.Existed)
	return pub, nil
}

// commitFiles publishes the files of st through Upload to repo, the repo of s, as the principal
// that opened s, with the parameters name, version and uploaded-by. Each file reads back from
// the registry if Build opens it.
func (p *Pipeline) commitFiles(ctx context.Context, repo index.Repo, s *index.Session, st *sessionState) (*format.Published, error) {
	principal, err := auth.ParsePrincipal(s.Principal)
	if err != nil {
		return nil, fmt.Errorf("ingest: session %d: %w", s.ID, err)
	}
	t, err := p.sessionTarget(ctx, s, st)
	if err != nil {
		return nil, err
	}
	// uploaded-by is the parameter of every format's ParamUploadedBy: the principal is the
	// session's, who sent the files.
	up := format.Upload{Params: map[string]string{"name": st.Name, "version": st.Version, "uploaded-by": s.Principal}}
	for _, f := range st.Files {
		d := ocispec.Descriptor{MediaType: sessionBlobType, Digest: f.Digest, Size: f.Size}
		up.Files = append(up.Files, format.UploadedFile{Name: f.Name, Size: f.Size, Checksums: f.Checksums,
			Open: func() (io.ReadCloser, error) { return t.rr.Fetch(ctx, d) }})
	}
	return p.Upload(ctx, repo, up, principal)
}

// Abort implements format.Sessions. A session being committed is not aborted: ErrConflict.
func (p *Pipeline) Abort(ctx context.Context, sessionID int64) error {
	s, err := p.index.GetSessionByID(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("ingest: session %d: %w", sessionID, err)
	}
	if s.State != sessionOpen {
		return nil
	}
	st, err := decodeSession(s)
	if err != nil {
		return err
	}
	if !st.Committing.IsZero() && p.now().Sub(st.Committing) < p.claimTTL {
		return fmt.Errorf("%w: session %d is being committed", ErrConflict, sessionID)
	}
	s.State = sessionAborted
	if err := p.index.UpsertSession(ctx, s); err != nil {
		return fmt.Errorf("ingest: abort session %d: %w", sessionID, err)
	}
	p.log.InfoContext(ctx, "ingest: session aborted", "session", sessionID, "key", s.Key, "files", len(st.Files))
	return nil
}

// ExpireSessions marks the open sessions whose time is up as expired, those of every kind
// (blob uploads of the facade too), and returns how many it changed. Nothing depends on it
// running: Open, Add and Commit treat a session whose time is up as expired by themselves; it
// keeps the states of the rows true for whoever reads them.
func (p *Pipeline) ExpireSessions(ctx context.Context) (int, error) {
	n, err := p.index.ExpireSessions(ctx, p.now())
	if err != nil {
		return 0, fmt.Errorf("ingest: expire sessions: %w", err)
	}
	return n, nil
}

// liveSession returns the session id and its state when it is open and its time is not up, and
// index.ErrNotFound otherwise.
func (p *Pipeline) liveSession(ctx context.Context, id int64) (*index.Session, *sessionState, error) {
	s, err := p.index.GetSessionByID(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("ingest: session %d: %w", id, err)
	}
	if s.State != sessionOpen || !p.now().Before(s.ExpiresAt) {
		return nil, nil, fmt.Errorf("%w: session %d is not open", index.ErrNotFound, id)
	}
	st, err := decodeSession(s)
	if err != nil {
		return nil, nil, err
	}
	return s, st, nil
}

// changeSession applies change to the state of the open session id in one transaction that
// starts with TouchSession, which also moves its expiry timeout on: timeoutOf the session's
// repo, which the caller has read. A session that is not open, or whose time is up, is
// index.ErrNotFound; an error of change is returned as it is.
func (p *Pipeline) changeSession(ctx context.Context, id int64, timeout time.Duration, change func(*sessionState) error) error {
	return p.index.Tx(ctx, func(w index.Writer) error {
		now := p.now()
		s, err := w.TouchSession(ctx, id, now, now.Add(timeout))
		if errors.Is(err, index.ErrNotFound) {
			return fmt.Errorf("%w: session %d is not open", index.ErrNotFound, id)
		} else if err != nil {
			return fmt.Errorf("ingest: session %d: %w", id, err)
		}
		st, err := decodeSession(s)
		if err != nil {
			return err
		}
		if err := change(st); err != nil {
			return err
		}
		if s.Files, err = json.Marshal(st); err != nil {
			return fmt.Errorf("ingest: session %d: %w", id, err)
		}
		if err := w.UpsertSession(ctx, s); err != nil {
			return fmt.Errorf("ingest: session %d: %w", id, err)
		}
		return nil
	})
}

// timeoutOf is how long a session of repo lives after its last request: the repo policy's
// maven.sessionTimeout when it is a positive duration (the API checks it: 1m to 24h on a maven
// repo), else Options.SessionTimeout. The policy is read leniently: one that does not parse is
// none.
func (p *Pipeline) timeoutOf(repo index.Repo) time.Duration {
	var policy struct {
		Maven struct {
			SessionTimeout string `json:"sessionTimeout"`
		} `json:"maven"`
	}
	if len(repo.Policy) > 0 && json.Unmarshal(repo.Policy, &policy) == nil {
		if d, err := time.ParseDuration(policy.Maven.SessionTimeout); err == nil && d > 0 {
			return d
		}
	}
	return p.sessionTimeout
}

// maxFilesOf is how many files a session of repo holds: the repo policy's maven.maxSessionFiles
// when it is positive (the API checks it: 1 to 999 on a maven repo), else
// defaultMaxSessionFiles. The policy is read leniently, as timeoutOf reads it.
func maxFilesOf(repo index.Repo) int {
	var policy struct {
		Maven struct {
			MaxSessionFiles int `json:"maxSessionFiles"`
		} `json:"maven"`
	}
	if len(repo.Policy) > 0 && json.Unmarshal(repo.Policy, &policy) == nil && policy.Maven.MaxSessionFiles > 0 {
		return policy.Maven.MaxSessionFiles
	}
	return defaultMaxSessionFiles
}

// sessionTarget is the OCI repository of the version a session publishes: where Add pushes
// its files, and where the manifest goes.
func (p *Pipeline) sessionTarget(ctx context.Context, s *index.Session, st *sessionState) (*target, error) {
	repo, err := p.index.GetRepoByID(ctx, s.RepoID)
	if err != nil {
		return nil, fmt.Errorf("ingest: repo of session %d: %w", s.ID, err)
	}
	plugin, prov, err := p.lookup(*repo)
	if err != nil {
		return nil, err
	}
	pkgPath, err := plugin.EscapeName(st.Name)
	if err != nil {
		return nil, invalid(fmt.Errorf("ingest: session %d of %s: %w", s.ID, st.Name, err))
	}
	if pkgPath, err = p.storedPath(ctx, *repo, pkgPath); err != nil {
		return nil, err
	}
	t := &target{repo: *repo, plugin: plugin, pkgPath: pkgPath, name: ociName(*repo, pkgPath), prov: prov}
	if t.rr, err = prov.Repository(ctx, t.name); err != nil {
		return nil, fmt.Errorf("ingest: repository %s: %w", t.name, err)
	}
	return t, nil
}

// decodeSession reads the state of a publication session's row.
func decodeSession(s *index.Session) (*sessionState, error) {
	var st sessionState
	if err := json.Unmarshal(s.Files, &st); err != nil {
		return nil, fmt.Errorf("ingest: session %d: the files column is not a publication session's: %w", s.ID, err)
	}
	return &st, nil
}

// spoolHashed copies r to a temporary file while it computes the file's digest and checksums,
// and returns the file, positioned anywhere, for the caller to close and remove. size is the
// length the client declared, -1 when unknown; another length is ErrInvalid. It is a request
// spool, never artifact storage (CLAUDE.md, code rules): the file lives as long as Add.
func spoolHashed(r io.Reader, name string, size int64) (*os.File, format.SessionFile, error) {
	f, err := os.CreateTemp("", "lajstrom-session-*")
	if err != nil {
		return nil, format.SessionFile{}, fmt.Errorf("ingest: spool %s: %w", name, err)
	}
	fail := func(err error) (*os.File, format.SessionFile, error) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, format.SessionFile{}, err
	}
	sums := map[string]hash.Hash{"md5": md5.New(), "sha1": sha1.New(), "sha256": sha256.New(), "sha512": sha512.New()} //nolint:gosec // maven's checksums
	w := []io.Writer{f}
	for _, h := range sums {
		w = append(w, h)
	}
	n, err := io.Copy(io.MultiWriter(w...), r)
	switch {
	case err != nil:
		return fail(fmt.Errorf("ingest: spool %s: %w", name, err))
	case size >= 0 && n != size:
		return fail(fmt.Errorf("%w: %s has %d bytes, Content-Length says %d", ErrInvalid, name, n, size))
	}
	sf := format.SessionFile{Name: name, Size: n, Checksums: map[string]string{}}
	for alg, h := range sums {
		sf.Checksums[alg] = hex.EncodeToString(h.Sum(nil))
	}
	sf.Digest = digest.NewDigestFromEncoded(digest.SHA256, sf.Checksums["sha256"])
	return f, sf, nil
}
