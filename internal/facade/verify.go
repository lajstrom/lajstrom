package facade

import (
	"context"
	"crypto/sha256"
	"encoding"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// The facade checks the digest of a blob that a client uploads (#368) instead of trusting the
// backend: JFrog JCR stores whatever it is sent under the digest it is told. The bytes are
// hashed as they stream, and the last byte of a blob is held back until the hash is known, so
// that a blob without its digest never completes in the backend. Between the chunks of a
// session the sha256 state is in its row (uploadState), so any replica continues it. A mount
// streams nothing and is not hashed: it relies on the backend, as a blob the client skips does.

// errNotDigest is what the backend's request gets instead of a blob's last byte, and the cause
// of the 400 DIGEST_INVALID, when the bytes do not have the digest the client named.
var errNotDigest = errors.New("facade: the uploaded bytes do not have the digest")

// errUnchecked is the answer to an upload whose backend holds bytes that did not pass through
// the hash, part of a chunk that failed: the session ends.
var errUnchecked = errors.New("facade: the registry holds bytes of this upload that Lajstrom did not check, " +
	"after a chunk that failed: start the upload again")

// verifier passes the body of a blob's last request on, hashing it, and holds its last byte
// back until the body ends: it is released, and then the end, only when the hash h of the whole
// blob is want. Otherwise the reader fails with errNotDigest one byte short, and the backend's
// request breaks off short of its length, or of its chunked end, which no registry takes as a
// blob. Nothing is buffered but that byte.
type verifier struct {
	r       io.Reader
	h       hash.Hash // the hash of the blob's bytes before r, and of r's as they are read
	want    digest.Digest
	held    byte
	holding bool
	end     error // nil while r has bytes; io.EOF after a match, errNotDigest, or r's error
}

func (v *verifier) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for v.end == nil {
		n, err := v.next(p)
		if err != nil {
			v.end = err
			if errors.Is(err, io.EOF) && digest.NewDigest(v.want.Algorithm(), v.h) != v.want {
				v.end = errNotDigest
			}
		}
		if n > 0 {
			return n, nil
		}
	}
	if v.holding && errors.Is(v.end, io.EOF) {
		p[0], v.holding = v.held, false
		return 1, nil
	}
	return 0, v.end
}

// next reads r into p and returns how many bytes of p to pass on: the byte held back before, if
// any, and those read but the last, which is held back in turn.
func (v *verifier) next(p []byte) (int, error) {
	buf := p
	if v.holding {
		buf = p[1:]
	}
	if len(buf) == 0 { // room for the held byte only
		var one [1]byte
		n, err := v.r.Read(one[:])
		if n == 0 {
			return 0, err
		}
		_, _ = v.h.Write(one[:])
		p[0], v.held = v.held, one[0]
		return 1, err
	}
	n, err := v.r.Read(buf)
	if n == 0 {
		return 0, err
	}
	_, _ = v.h.Write(buf[:n]) // a hash.Hash never fails
	if v.holding {
		p[0] = v.held
		n++
	}
	v.held, v.holding = p[n-1], true
	return n - 1, err
}

// finishChecked closes the backend session u with the digest d after the last chunk body (nil:
// none) of size bytes, which a verifier passes on; h is the hash of the blob's bytes before
// body. It returns errNotDigest when the blob does not have the digest d: without a body
// before the backend is asked, with one before the backend has its last byte.
func finishChecked(ctx context.Context, rr provider.Repository, u provider.Upload, d digest.Digest, h hash.Hash, body *bodyReader, size int64) error {
	if body == nil {
		if digest.NewDigest(d.Algorithm(), h) != d {
			return errNotDigest
		}
		_, err := rr.FinishBlobUpload(ctx, u, d, nil, size)
		return err
	}
	v := &verifier{r: body, h: h, want: d}
	_, err := rr.FinishBlobUpload(ctx, u, d, v, size)
	if errors.Is(v.end, errNotDigest) {
		return errNotDigest
	}
	return err
}

// closingHash returns the hash with which the upload st is closed with the digest d: a new one
// of d's algorithm when no bytes have passed yet, else the sha256 of those that have. When a
// chunk failed since (Recheck), or the backend's offset is not what passed, the backend is asked
// first, and holding other bytes than those is errUnchecked. A session with bytes closed with a
// digest that is not sha256 cannot be checked: DIGEST_INVALID. A status the backend does not
// answer is returned as it is.
func closingHash(ctx context.Context, rr provider.Repository, st uploadState, d digest.Digest) (hash.Hash, error) {
	if st.Recheck || st.Offset != st.Hashed {
		u, err := rr.BlobUploadStatus(ctx, st.upload())
		if err != nil {
			return nil, err
		}
		if u.Offset != st.Hashed {
			return nil, &apiError{status: http.StatusBadRequest, code: codeDigestInvalid, msg: errUnchecked.Error(), err: errUnchecked}
		}
	}
	if st.Hashed == 0 {
		return d.Algorithm().Hash(), nil
	}
	if d.Algorithm() != digest.SHA256 {
		return nil, &apiError{status: http.StatusBadRequest, code: codeDigestInvalid,
			msg: fmt.Sprintf("a blob uploaded in chunks is checked in sha256, not %s: send it in one request", d.Algorithm())}
	}
	return st.sha256()
}

// sha256 returns the sha256 of the bytes that passed in the session st, from its state in the
// row (crypto/sha256's hash implements encoding.BinaryMarshaler and BinaryUnmarshaler): a new
// one before any.
func (st uploadState) sha256() (hash.Hash, error) {
	h := sha256.New()
	if st.SHA256 != nil {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(st.SHA256); err != nil {
			return nil, fmt.Errorf("facade: upload session: sha256 state: %w", err)
		}
	}
	return h, nil
}
