package format

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/index"
)

// MaxFileSize returns policy.maxFileSize of the repo, an integer number of bytes, or -1 when the
// policy sets none (docs/api.md, the policy keys). A value that is not a non-negative integer is
// an error, which StatusOf answers with 500: an upload is not let through a limit that cannot
// be read. Every frontend that takes an upload bounds it with LimitBody, which reads this.
func MaxFileSize(repo index.Repo) (int64, error) {
	if len(repo.Policy) == 0 {
		return -1, nil
	}
	var p struct {
		MaxFileSize *int64 `json:"maxFileSize"`
	}
	err := json.Unmarshal(repo.Policy, &p)
	switch {
	case err != nil:
		return 0, fmt.Errorf("format: repo %s/%s: policy maxFileSize: %w", repo.Format, repo.Name, err)
	case p.MaxFileSize == nil:
		return -1, nil
	case *p.MaxFileSize < 0:
		return 0, fmt.Errorf("format: repo %s/%s: policy maxFileSize %d is negative", repo.Format, repo.Name, *p.MaxFileSize)
	}
	return *p.MaxFileSize, nil
}

// LimitBody returns the body of the upload r bounded by MaxFileSize of the repo with
// http.MaxBytesReader, or as it is when the policy sets none. A declared Content-Length above
// the limit is refused before anything is read. Past the limit, the reader fails with an error
// that names the repo and its limit and wraps the *http.MaxBytesError, so that the error of
// Spool says why and StatusOf answers 413. The error of LimitBody itself is that 413, or the
// 500 of a policy MaxFileSize cannot read.
func LimitBody(w http.ResponseWriter, r *http.Request, repo index.Repo) (io.Reader, error) {
	limit, err := MaxFileSize(repo)
	if err != nil || limit < 0 {
		return r.Body, err
	}
	if r.ContentLength > limit {
		return nil, tooLarge(repo, &http.MaxBytesError{Limit: limit})
	}
	return &limitedBody{r: http.MaxBytesReader(w, r.Body, limit), repo: repo}, nil
}

// limitedBody is the reader of LimitBody: the errors of r pass through tooLarge.
type limitedBody struct {
	r    io.Reader
	repo index.Repo
}

func (b *limitedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil {
		err = tooLarge(b.repo, err)
	}
	return n, err
}

// tooLarge names the repo's limit in an error that wraps an *http.MaxBytesError, and returns any
// other error, io.EOF among them, unchanged.
func tooLarge(repo index.Repo, err error) error {
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		return err
	}
	return fmt.Errorf("the file is larger than the maxFileSize of %s/%s, %d bytes: %w", repo.Format, repo.Name, mbe.Limit, err)
}
