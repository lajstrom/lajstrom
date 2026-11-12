package rpm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The signed document of a repo and its keyring (docs/spec/http-routing.md §3, rpm).
const (
	kindRepomdASC = "repomd.xml.asc"
	kindRepomdKey = "repomd.xml.key"

	signatureContentType = "application/pgp-signature"
	keysContentType      = "application/pgp-keys"

	// signRounds bounds the rounds of handlers.signed; a round is lost when the repodata is
	// rendered again between the read of its repomd.xml and the store of the signature.
	signRounds = 3
)

// repodata serves repodata/{file}: repomd.xml, from index_cache or rendered with the three
// documents it lists; repomd.xml.asc, the detached signature of the same bytes; repomd.xml.key,
// the keyring; and a document the current repomd.xml lists by its location. A name it does not
// list is 404 without a render: the documents are stored together, so repomd.xml is the list of
// what the cache holds, and a request for an older render's file does not render on every try.
// Without a Signer, repomd.xml.asc and repomd.xml.key are 404, and clients set repo_gpgcheck=0.
func (h *handlers) repodata(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r, auth.ActionRead)
	if !ok {
		return
	}
	ctx := r.Context()
	switch file := r.PathValue("file"); file {
	case kindRepomdKey:
		h.publicKeys(w, r, repo)
	case kindRepomdASC:
		if h.deps.Signer == nil {
			h.fail(w, r, unsigned(repo, scopeRepodata+"/"+file))
			return
		}
		c, err := h.signed(ctx, repo)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		format.ServeDocument(w, r, lastSigned(c), signatureContentType)
	default:
		c, err := h.cached(ctx, repo, kindRepomd)
		if err == nil && file != kindRepomd {
			if !listed(c.Content)[file] {
				err = fmt.Errorf("%w: rpm/%s has no repodata/%s in the current repomd.xml", index.ErrNotFound, repo.Name, file)
			} else {
				c, err = h.cached(ctx, repo, file)
			}
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		ct := gzipContentType
		if file == kindRepomd {
			ct = xmlContentType
		}
		format.ServeDocument(w, r, c, ct)
	}
}

// keyring serves key.asc, the same keyring as repodata/repomd.xml.key.
func (h *handlers) keyring(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r, auth.ActionRead)
	if !ok {
		return
	}
	h.publicKeys(w, r, repo)
}

// publicKeys answers the armored keyring of the repo from the Signer: its active key (made on
// the first request) and the keys retired within keys.retireGrace, what gpgkey= points at. It
// is not cached: it is small, and a rotation changes it. Without a Signer it is 404.
func (h *handlers) publicKeys(w http.ResponseWriter, r *http.Request, repo index.Repo) {
	if h.deps.Signer == nil {
		h.fail(w, r, unsigned(repo, r.URL.Path))
		return
	}
	b, err := h.deps.Signer.PublicKeys(r.Context(), repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sum := sha256.Sum256(b)
	format.ServeDocument(w, r, &index.Cache{Content: b, SHA256: hex.EncodeToString(sum[:])}, keysContentType)
}

// cached returns the document kind of the repodata from index_cache, rendered with the whole
// repodata on a miss (format.CachedDocument). Nothing here invalidates: ingest and delete
// invalidate every document of the repo in the transaction that changes its versions, so the
// first GET after a publish renders the repodata again.
func (h *handlers) cached(ctx context.Context, repo index.Repo, kind string) (*index.Cache, error) {
	return format.CachedDocument(ctx, h.deps.Index, h.plugin, repo, scopeRepodata, kind)
}

// signed returns repomd.xml.asc: the detached signatures of the bytes of the repomd.xml in
// index_cache, rendered first when it is not there, stored beside it with its RenderedAt and
// SignedAt the time of signing. A signature whose RenderedAt is not that of the repomd.xml is of
// an older render and is made again, and so is one that the Signer no longer calls current (a
// key that signed it has stopped signing, #577). The Signer signs outside any transaction of the
// index, as the apt routes do; the signature is stored only while the repomd.xml it signs is the
// one in index_cache, so a render that replaced it meanwhile is signed in the next round.
func (h *handlers) signed(ctx context.Context, repo index.Repo) (*index.Cache, error) {
	for range signRounds {
		md, err := h.cached(ctx, repo, kindRepomd)
		if err != nil {
			return nil, err
		}
		c, err := h.deps.Index.GetIndexCache(ctx, repo.ID, scopeRepodata, kindRepomdASC)
		switch {
		case err == nil && c.RenderedAt.Equal(md.RenderedAt):
			current, err := h.deps.Signer.Current(ctx, repo, c.Content)
			if err != nil {
				return nil, err
			}
			if current {
				return c, nil
			}
		case err != nil && !errors.Is(err, index.ErrNotFound):
			return nil, err
		}
		sig, err := h.deps.Signer.DetachSign(ctx, repo, md.Content)
		if err != nil {
			return nil, err
		}
		c, err = h.storeSigned(ctx, md, sig)
		if !errors.Is(err, errRenderedAgain) {
			return c, err
		}
	}
	return nil, fmt.Errorf("rpm: rpm/%s repodata/%s: the repomd.xml was rendered again while it was signed, %d times",
		repo.Name, kindRepomdASC, signRounds)
}

// errRenderedAgain is the error of storeSigned when the repomd.xml it signed is no longer the
// one in index_cache, or an invalidation came while it stored the signature.
var errRenderedAgain = errors.New("rpm: the repomd.xml was rendered again")

// storeSigned stores the signature sig of the repomd.xml md in one transaction that first reads
// the repo's IndexCacheGeneration and checks that md is still the repomd.xml in index_cache.
func (h *handlers) storeSigned(ctx context.Context, md *index.Cache, sig []byte) (*index.Cache, error) {
	var stored *index.Cache
	err := h.deps.Index.Tx(ctx, func(tx index.Writer) error {
		gen, err := tx.IndexCacheGeneration(ctx, md.RepoID)
		if err != nil {
			return err
		}
		cur, err := tx.GetIndexCache(ctx, md.RepoID, md.Scope, kindRepomd)
		switch {
		case errors.Is(err, index.ErrNotFound):
			return errRenderedAgain
		case err != nil:
			return err
		case cur.SHA256 != md.SHA256 || !cur.RenderedAt.Equal(md.RenderedAt):
			return errRenderedAgain
		}
		signedAt := h.plugin.clock().UTC()
		sum := sha256.Sum256(sig)
		c := &index.Cache{RepoID: md.RepoID, Scope: md.Scope, Kind: kindRepomdASC, Content: sig, SHA256: hex.EncodeToString(sum[:]),
			RenderedAt: md.RenderedAt, SignedAt: &signedAt}
		ok, err := tx.PutIndexCache(ctx, c, gen)
		switch {
		case err != nil:
			return err
		case !ok:
			return errRenderedAgain
		}
		stored = c
		return nil
	})
	return stored, err
}

// lastSigned returns the signature c with its SignedAt as its RenderedAt, the Last-Modified of
// format.ServeDocument: a signature made again without a new render is newer than the
// repomd.xml it signs.
func lastSigned(c *index.Cache) *index.Cache {
	if c.SignedAt == nil || !c.SignedAt.After(c.RenderedAt) {
		return c
	}
	served := *c
	served.RenderedAt = *c.SignedAt
	return &served
}

// unsigned is the error for the signature or the keyring of a repo when the server does not
// sign.
func unsigned(repo index.Repo, path string) error {
	return fmt.Errorf("%w: rpm/%s: %s: this server does not sign, keys.masterKeyEnv is not set; use repo_gpgcheck=0",
		index.ErrNotFound, repo.Name, path)
}
