package raw

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// unread fails the test when it is read.
type unread struct{ t *testing.T }

func (u unread) Read([]byte) (int, error) {
	u.t.Error("the stream was read")
	return 0, io.EOF
}

// TestForwardSeekerSize checks what http.ServeContent does first: Seek to the end for the size
// and back to the start, without reading the stream.
func TestForwardSeekerSize(t *testing.T) {
	s := newForwardSeeker(unread{t}, 42)
	if n, err := s.Seek(0, io.SeekEnd); n != 42 || err != nil {
		t.Errorf("Seek(0, SeekEnd) = %d, %v", n, err)
	}
	if n, err := s.Seek(0, io.SeekStart); n != 0 || err != nil {
		t.Errorf("Seek(0, SeekStart) = %d, %v", n, err)
	}
	if _, err := s.Seek(0, 7); err == nil {
		t.Error("Seek with an invalid whence succeeded")
	}
	if _, err := s.Seek(-1, io.SeekStart); !errors.Is(err, errSeekBack) {
		t.Errorf("Seek(-1) = %v, want errSeekBack", err)
	}
}

// TestForwardSeekerRead checks a seek forwards, a relative one, the refusal of a seek back, and
// reading to the end.
func TestForwardSeekerRead(t *testing.T) {
	s := newForwardSeeker(strings.NewReader("0123456789"), 10)
	if _, err := s.Seek(3, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 2)
	if n, err := io.ReadFull(s, b); n != 2 || err != nil || string(b) != "34" {
		t.Errorf("read after Seek(3) = %q, %v", b[:n], err)
	}
	if n, err := s.Seek(2, io.SeekCurrent); n != 7 || err != nil {
		t.Errorf("Seek(2, SeekCurrent) = %d, %v", n, err)
	}
	if _, err := s.Seek(4, io.SeekStart); !errors.Is(err, errSeekBack) {
		t.Errorf("Seek back = %v, want errSeekBack", err)
	}
	rest, err := io.ReadAll(s)
	if err != nil || string(rest) != "789" || s.err != nil {
		t.Errorf("rest = %q, %v, recorded %v", rest, err, s.err)
	}
}

// failAfter returns data, then err.
type failAfter struct {
	data string
	err  error
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.data == "" {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

// TestForwardSeekerFailures checks that a stream shorter than the seek is ErrUnexpectedEOF and
// that the first error of the stream is recorded.
func TestForwardSeekerFailures(t *testing.T) {
	s := newForwardSeeker(strings.NewReader("012"), 10)
	_, _ = s.Seek(5, io.SeekStart)
	if _, err := s.Read(make([]byte, 4)); !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(s.err, io.ErrUnexpectedEOF) {
		t.Errorf("read past a short stream = %v, recorded %v", err, s.err)
	}
	reset := errors.New("connection reset")
	s = newForwardSeeker(&failAfter{data: "01", err: reset}, 10)
	if _, err := io.ReadAll(s); !errors.Is(err, reset) || !errors.Is(s.err, reset) {
		t.Errorf("ReadAll of a failing stream = %v, recorded %v", err, s.err)
	}
	s = newForwardSeeker(&failAfter{err: reset}, 10)
	_, _ = s.Seek(1, io.SeekStart)
	_, err := s.Read(make([]byte, 1))
	_ = s.fail(io.ErrClosedPipe) // a later failure does not replace the first
	if !errors.Is(err, reset) || !errors.Is(s.err, reset) {
		t.Errorf("discard on a failing stream = %v, recorded %v", err, s.err)
	}
}
