package format

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/index"
)

// defaultMaxFileSizes is MaxFileSize of a repo whose policy sets none, by format
// (docs/spec/manifest-profile.md §5, #246): an upload a frontend spools, and a blob of /v2, is
// bounded out of the box. The sizes are above the largest files of the package size survey of
// #439: npm's largest tarball 152 MiB, PyTorch's CUDA wheels 2.1 GiB, Maven Central's largest
// jar 2.3 GiB, .debs of 5 GiB, an image layer of 10.78 GiB (rocm/pytorch). ML models are larger
// still (a 70B model in fp16 is one blob of 131 GiB): their oci repos set a policy.
var defaultMaxFileSizes = map[string]int64{
	"npm":   512 << 20,
	"pypi":  4 << 30,
	"maven": 4 << 30,
	"apt":   8 << 30,
	"raw":   8 << 30,
	"oci":   16 << 30,
}

// defaultMaxFileSize is the default of a format defaultMaxFileSizes does not name.
const defaultMaxFileSize = 8 << 30

// defaultMaxFileSizeOf returns the maxFileSize of a repo of format whose policy sets none.
func defaultMaxFileSizeOf(format string) int64 {
	if n, ok := defaultMaxFileSizes[format]; ok {
		return n
	}
	return defaultMaxFileSize
}

// MaxFileSize returns policy.maxFileSize of the repo, an integer number of bytes, or the default
// of the repo's format when the policy sets none (npm 512 MiB, pypi and maven 4 GiB, apt, raw and
// any other 8 GiB, oci 16 GiB; docs/api.md, the policy keys). A value that is not a non-negative
// integer is an error, which StatusOf answers with 500: an upload is not let through a limit that
// cannot be read. Every frontend that takes an upload bounds it with LimitBody, which reads this,
// and the facade a /v2 blob, of an oci repo too.
func MaxFileSize(repo index.Repo) (int64, error) {
	if len(repo.Policy) == 0 {
		return defaultMaxFileSizeOf(repo.Format), nil
	}
	var p struct {
		MaxFileSize *int64 `json:"maxFileSize"`
	}
	err := json.Unmarshal(repo.Policy, &p)
	switch {
	case err != nil:
		return 0, fmt.Errorf("format: repo %s/%s: policy maxFileSize: %w", repo.Format, repo.Name, err)
	case p.MaxFileSize == nil:
		return defaultMaxFileSizeOf(repo.Format), nil
	case *p.MaxFileSize < 0:
		return 0, fmt.Errorf("format: repo %s/%s: policy maxFileSize %d is negative", repo.Format, repo.Name, *p.MaxFileSize)
	}
	return *p.MaxFileSize, nil
}

// LimitBody returns the body of the upload r bounded by MaxFileSize of the repo with
// http.MaxBytesReader. A declared Content-Length above the limit is refused before anything is
// read. Past the limit, the reader fails with an error that names the repo and its limit and
// wraps the *http.MaxBytesError, so that the error of Spool says why and StatusOf answers 413.
// The error of LimitBody itself is that 413, or the 500 of a policy MaxFileSize cannot read.
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
