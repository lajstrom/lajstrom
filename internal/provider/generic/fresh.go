package generic

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// freshSession is how long after the POST that opened an upload session a BLOB_UPLOAD_UNKNOWN
// of it is taken for a registry that does not serve the session yet rather than one that
// dropped it, on a registry not classified strong: ingest's visibility timeout by default, or
// the registry's consistencyBound when that is longer. ECR may not know a session for a moment
// after the 202 of its POST (#446); every registry keeps a session for minutes.
const freshSession = 30 * time.Second

// freshUploads is the BlobUploader of one blob writer (OpenBlobWriter): the repository's upload
// session, whose first write is sent again while the registry answers BLOB_UPLOAD_UNKNOWN within
// freshSession of its POST (sendFresh). The writer holds the chunk in memory, so it is sent
// again from its start. A write after the registry took bytes into the session is sent once:
// the registry knew the session then.
type freshUploads struct {
	*repository
	opened time.Time // when StartBlobUpload opened the session; zero before
}

func (u *freshUploads) StartBlobUpload(ctx context.Context) (provider.Upload, error) {
	up, err := u.repository.StartBlobUpload(ctx)
	if err == nil {
		u.opened = time.Now()
	}
	return up, err
}

func (u *freshUploads) AppendBlobChunk(ctx context.Context, up provider.Upload, body io.Reader, size int64) (provider.Upload, error) {
	next := up
	err := u.sendFresh(ctx, u.since(up), body, func() (err error) {
		next, err = u.repository.AppendBlobChunk(ctx, up, body, size)
		return err
	})
	return next, err
}

func (u *freshUploads) FinishBlobUpload(ctx context.Context, up provider.Upload, d digest.Digest, body io.Reader, size int64) (ocispec.Descriptor, error) {
	var desc ocispec.Descriptor
	err := u.sendFresh(ctx, u.since(up), body, func() (err error) {
		desc, err = u.repository.FinishBlobUpload(ctx, up, d, body, size)
		return err
	})
	return desc, err
}

// since is when the session up was opened while it holds no bytes; zero once it holds some.
func (u *freshUploads) since(up provider.Upload) time.Time {
	if up.Offset > 0 {
		return time.Time{}
	}
	return u.opened
}

// sendFresh calls send, a write to an upload session opened at opened, and again while it fails
// with BLOB_UPLOAD_UNKNOWN on a registry not classified strong, with the backoff of a read-back,
// until freshSession after opened, or the registry's consistencyBound when that is longer; body
// is rewound to where it started before each. A zero opened, a strong registry and a body that
// cannot be rewound (not an io.Seeker: a client's stream) send once.
func (r *repository) sendFresh(ctx context.Context, opened time.Time, body io.Reader, send func() error) error {
	rewind := rewinder(body)
	err := send()
	if opened.IsZero() || rewind == nil || !uploadUnknown(err) {
		return err
	}
	caps, cerr := r.p.Capabilities(ctx)
	if cerr != nil || caps.Consistency == provider.ConsistencyStrong {
		return err
	}
	again := false
	deadline := opened.Add(max(freshSession, caps.ConsistencyBound))
	_, werr := provider.ReadBack{Timeout: time.Until(deadline)}.Await(ctx, func(context.Context) (bool, error) {
		if !again { // the first answer is the send above: wait before the next
			again = true
			return false, nil
		}
		if !rewind() {
			return true, nil
		}
		err = send()
		return !uploadUnknown(err), nil
	})
	if werr != nil {
		return werr
	}
	return err
}

// rewinder returns what rewinds body to where it is now, or nil when it cannot; a nil body needs
// no rewind.
func rewinder(body io.Reader) func() bool {
	if body == nil {
		return func() bool { return true }
	}
	s, ok := body.(io.Seeker)
	if !ok {
		return nil
	}
	start, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil
	}
	return func() bool {
		_, err := s.Seek(start, io.SeekStart)
		return err == nil
	}
}

// uploadUnknown reports whether err is the registry's BLOB_UPLOAD_UNKNOWN.
func uploadUnknown(err error) bool {
	var er *errcode.ErrorResponse
	return errors.As(err, &er) && hasCode(er, errcode.ErrorCodeBlobUploadUnknown)
}
