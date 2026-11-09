package format

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
)

// Spooled is an upload body copied to a temporary file for the duration of one request, so that
// it can be read more than once: Plugin.Build reads it to digest it, ingest reads it again to
// push the blob. It is a request spool, never artifact storage: nothing serves from it, and the
// handler closes it, which removes the file, when the request ends (CLAUDE.md, code rules).
type Spooled struct {
	f           *os.File
	path        string
	name        string
	contentType string
	size        int64

	once     sync.Once
	closeErr error
}

// Spool copies r to a new temporary file in os.TempDir(), $TMPDIR when it is set (decided on
// #58: no configuration key of its own), and returns it as a Spooled of name and contentType. size is the length the
// client declared, -1 when unknown; when it is known and r ends at another length, the error
// wraps ErrInvalid. Nothing is hashed here: Build digests the file. The file is unlinked right
// after it is created where the system allows it, so that a crash leaves nothing behind; Close
// removes it otherwise.
//
// Errors of r are returned wrapped with %w: a body wrapped with http.MaxBytesReader that is too
// large surfaces as an *http.MaxBytesError, which StatusOf answers with 413. On error no file is
// left.
func Spool(r io.Reader, name, contentType string, size int64) (*Spooled, error) {
	f, err := os.CreateTemp("", "lajstrom-upload-*")
	if err != nil {
		return nil, fmt.Errorf("format: spool %s: %w", name, err)
	}
	s := &Spooled{f: f, path: f.Name(), name: name, contentType: contentType}
	_ = os.Remove(s.path) // best effort: an open file cannot be removed everywhere
	n, err := io.Copy(f, r)
	if err == nil && size >= 0 && n != size {
		err = fmt.Errorf("%w: the body has %d bytes, Content-Length says %d", ErrInvalid, n, size)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("format: spool %s: %w", name, err), s.Close())
	}
	s.size = n
	return s, nil
}

// File returns the spooled body as the UploadedFile of an Upload: Size is the number of bytes
// spooled, and every call of Open returns a new reader from the start of the file. The readers
// read at offsets, so several may be open at once; they are valid until Close.
func (s *Spooled) File() UploadedFile {
	return UploadedFile{
		Name:        s.name,
		Size:        s.size,
		ContentType: s.contentType,
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(s.f, 0, s.size)), nil
		},
	}
}

// Close closes and removes the temporary file. It is safe to call more than once; the later
// calls return the first call's error.
func (s *Spooled) Close() error {
	s.once.Do(func() {
		err := s.f.Close()
		if rerr := os.Remove(s.path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
		if err != nil {
			s.closeErr = fmt.Errorf("format: spool %s: %w", s.name, err)
		}
	})
	return s.closeErr
}
