package raw

import (
	"errors"
	"fmt"
	"io"
)

// errSeekBack is the error of a seek before what the stream has already passed.
var errSeekBack = errors.New("raw: the blob stream cannot seek backwards")

// forwardSeeker is the io.ReadSeeker http.ServeContent needs, over a stream of known size that
// can only go forwards: the blob as the backend sends it. Seek answers the size for SeekEnd
// without reading, and a seek forwards only records the offset: the next Read discards the
// bytes up to it. A seek before the bytes already read fails. ServeContent seeks to the end and
// back to the start to learn the size, and to the start of a single range; a multi-range request
// would seek backwards, which is why the routes drop it. err keeps the first failure of the
// stream, for the log: ServeContent has written the headers by then.
type forwardSeeker struct {
	r    io.Reader
	size int64
	pos  int64 // bytes taken from r
	off  int64 // where the next Read starts; at least pos
	err  error
}

// newForwardSeeker returns a forwardSeeker over r, whose length is size.
func newForwardSeeker(r io.Reader, size int64) *forwardSeeker {
	return &forwardSeeker{r: r, size: size}
}

// Seek implements io.Seeker.
func (s *forwardSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = s.off + offset
	case io.SeekEnd:
		abs = s.size + offset
	default:
		return 0, fmt.Errorf("raw: seek: invalid whence %d", whence)
	}
	if abs < s.pos {
		return 0, fmt.Errorf("%w: to %d after %d bytes", errSeekBack, abs, s.pos)
	}
	s.off = abs
	return abs, nil
}

// Read implements io.Reader: it discards the bytes up to the offset of the last seek, then
// reads from the stream.
func (s *forwardSeeker) Read(p []byte) (int, error) {
	if s.off > s.pos {
		n, err := io.CopyN(io.Discard, s.r, s.off-s.pos)
		s.pos += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return 0, s.fail(err)
		}
	}
	n, err := s.r.Read(p)
	s.pos += int64(n)
	s.off = s.pos
	if err != nil && !errors.Is(err, io.EOF) {
		return n, s.fail(err)
	}
	return n, err
}

// fail records the first failure of the stream and returns err.
func (s *forwardSeeker) fail(err error) error {
	if s.err == nil {
		s.err = err
	}
	return err
}
