package npm

import (
	"bufio"
	"bytes"
	"crypto/sha1" //nolint:gosec // npm's dist.shasum is a sha1, checked, not trusted
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/lajstrom/lajstrom/internal/format"
)

// maxBody bounds a publish body besides the data of its attachment: the version document, the
// readme, the rest (docs/spec/http-routing.md, npm).
const maxBody = 4 << 20

// keyAttachments is the member of a publish body with the tarball.
const keyAttachments = "_attachments"

// publishBody is npm's publish body as readBody read it: every member but _attachments as JSON,
// and the attachments with their data decoded into spool files. Close removes the files.
type publishBody struct {
	fields      map[string]json.RawMessage
	attachments []*attachment
}

// attachment is a member of _attachments: its name, its length as sent (nil when absent), its
// data decoded into a spool file, and the digests of the data.
type attachment struct {
	name                 string
	length               json.RawMessage
	file                 *format.Spooled
	sha256, sha512, sha1 []byte
}

// Close removes the spool files of the attachments.
func (b *publishBody) Close() error {
	var errs []error
	for _, a := range b.attachments {
		if a.file != nil {
			errs = append(errs, a.file.Close())
		}
	}
	return errors.Join(errs...)
}

// readBody reads npm's publish body from r as it streams: one JSON object whose members are
// kept as JSON, at most maxBody bytes of them, but _attachments, whose one member's data, the
// tarball in base64, is decoded into a spool file as it comes, never held whole, at most limit
// bytes of it (the repo's maxFileSize; -1 for none). A second attachment is refused before its
// data is read. Errors wrap format.ErrInvalid, or an *http.MaxBytesError over a bound (413),
// naming it with what; on error no spool file is left.
func readBody(r io.Reader, limit int64, what string) (_ *publishBody, err error) {
	s := &scanner{r: bufio.NewReaderSize(r, 64<<10), left: maxBody}
	b := &publishBody{fields: map[string]json.RawMessage{}}
	defer func() {
		if err != nil {
			_ = b.Close()
		}
	}()
	if err := s.expect('{'); err != nil {
		return nil, err
	}
	err = s.members(func(key string) error {
		if key != keyAttachments {
			v, err := s.value()
			b.fields[key] = v
			return err
		}
		if b.attachments != nil {
			return fmt.Errorf("%w: the publish body has %s twice", format.ErrInvalid, keyAttachments)
		}
		b.attachments = []*attachment{}
		return s.attachments(b, limit, what)
	})
	if err != nil {
		return nil, err
	}
	if c, err := s.token(); err == nil {
		return nil, fmt.Errorf("%w: the publish body has %q after its object", format.ErrInvalid, c)
	} else if !errors.Is(err, errEnd) {
		return nil, err
	}
	return b, nil
}

// attachments reads the members of _attachments into b.
func (s *scanner) attachments(b *publishBody, limit int64, what string) error {
	if err := s.expect('{'); err != nil {
		return err
	}
	return s.members(func(name string) error {
		if len(b.attachments) > 0 {
			return fmt.Errorf("%w: the publish body has more than one attachment, %q and %q; one tarball is one publish", format.ErrInvalid,
				b.attachments[0].name, name)
		}
		a := &attachment{name: name}
		b.attachments = append(b.attachments, a)
		if err := s.expect('{'); err != nil {
			return err
		}
		return s.members(func(key string) error {
			switch key {
			case "data":
				if a.file != nil {
					return fmt.Errorf("%w: the attachment %q has data twice", format.ErrInvalid, name)
				}
				if err := s.expect('"'); err != nil {
					return err
				}
				return a.spool(s.r, limit, what)
			case "length":
				var err error
				a.length, err = s.value()
				return err
			default:
				_, err := s.value()
				return err
			}
		})
	})
}

// spool decodes the base64 of the JSON string that r is in, its opening quote read, into a spool
// file, with its digests.
func (a *attachment) spool(r *bufio.Reader, limit int64, what string) error {
	h256, h512, h1 := sha256.New(), sha512.New(), sha1.New() //nolint:gosec // see the import
	data := io.Reader(base64.NewDecoder(base64.StdEncoding, &stringReader{r: r}))
	if limit >= 0 {
		data = &capped{r: data, left: limit, err: fmt.Errorf("the tarball is larger than the maxFileSize of %s, %d bytes: %w", what, limit,
			&http.MaxBytesError{Limit: limit})}
	}
	sp, err := format.Spool(io.TeeReader(data, io.MultiWriter(h256, h512, h1)), a.name, "application/octet-stream", -1)
	var corrupt base64.CorruptInputError
	switch {
	case errors.As(err, &corrupt):
		return fmt.Errorf("%w: the data of the attachment %q is not base64: %w", format.ErrInvalid, a.name, err)
	case err != nil:
		return bodyError(err)
	}
	a.file, a.sha256, a.sha512, a.sha1 = sp, h256.Sum(nil), h512.Sum(nil), h1.Sum(nil)
	return nil
}

// errEnd is the end of the body where a token could come.
var errEnd = errors.New("the end of the body")

// bodyError keeps an error over a bound (413) as it is and makes any other error of the body a
// 400, as format.ErrInvalid.
func bodyError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || errors.Is(err, format.ErrInvalid) {
		return err
	}
	return fmt.Errorf("%w: the publish body: %w", format.ErrInvalid, err)
}

// scanner reads the JSON of a publish body a byte at a time, at most left bytes of it outside the
// data of the attachment.
type scanner struct {
	r    *bufio.Reader
	left int64
}

// next returns the next byte; errEnd at the end of the body.
func (s *scanner) next() (byte, error) {
	c, err := s.r.ReadByte()
	switch {
	case errors.Is(err, io.EOF):
		return 0, errEnd
	case err != nil:
		return 0, bodyError(err)
	}
	if s.left--; s.left < 0 {
		return 0, fmt.Errorf("the publish body besides the tarball is more than %d bytes: %w", maxBody, &http.MaxBytesError{Limit: maxBody})
	}
	return c, nil
}

// token returns the next byte that is not JSON white space; errEnd at the end of the body.
func (s *scanner) token() (byte, error) {
	for {
		c, err := s.next()
		if err != nil || (c != ' ' && c != '\t' && c != '\n' && c != '\r') {
			return c, err
		}
	}
}

// expect reads the next token, which must be want.
func (s *scanner) expect(want byte) error {
	c, err := s.token()
	switch {
	case errors.Is(err, errEnd):
		return fmt.Errorf("%w: the publish body ends where %q should come", format.ErrInvalid, want)
	case err != nil:
		return err
	case c != want:
		return fmt.Errorf("%w: the publish body has %q where %q should come", format.ErrInvalid, c, want)
	}
	return nil
}

// members reads the members of an object whose { was read, and calls fn with each name after its
// colon, for fn to read the value.
func (s *scanner) members(fn func(key string) error) error {
	c, err := s.token()
	if err == nil && c == '}' {
		return nil
	}
	for {
		key, nameErr := s.name(c, err)
		if nameErr != nil {
			return nameErr
		}
		if err := s.expect(':'); err != nil {
			return err
		}
		if err := fn(key); err != nil {
			return err
		}
		switch c, err = s.token(); {
		case err != nil:
			return s.ended(err)
		case c == '}':
			return nil
		case c != ',':
			return fmt.Errorf("%w: the publish body has %q after a member", format.ErrInvalid, c)
		}
		c, err = s.token()
	}
}

// name reads a member name whose first byte c was read, or failed to be with err.
func (s *scanner) name(c byte, err error) (string, error) {
	switch {
	case err != nil:
		return "", s.ended(err)
	case c != '"':
		return "", fmt.Errorf("%w: the publish body has %q where a member name should come", format.ErrInvalid, c)
	}
	raw, err := s.value('"')
	if err != nil {
		return "", err
	}
	var key string
	if err := json.Unmarshal(raw, &key); err != nil {
		return "", fmt.Errorf("%w: a member name: %w", format.ErrInvalid, err)
	}
	return key, nil
}

// value reads one JSON value and returns it; first, when given, is its first byte, read already.
func (s *scanner) value(first ...byte) (json.RawMessage, error) {
	var c byte
	var err error
	if len(first) > 0 {
		c = first[0]
	} else if c, err = s.token(); err != nil {
		return nil, s.ended(err)
	}
	var buf bytes.Buffer
	for depth := 0; ; {
		buf.WriteByte(c)
		switch c {
		case '"':
			if err := s.rest(&buf); err != nil {
				return nil, err
			}
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		default:
			if depth == 0 {
				if err := s.scalar(&buf); err != nil {
					return nil, err
				}
			}
		}
		if depth <= 0 {
			break
		}
		if c, err = s.next(); err != nil {
			return nil, s.ended(err)
		}
	}
	if !json.Valid(buf.Bytes()) {
		return nil, fmt.Errorf("%w: the publish body has a value that is not JSON: %.40q", format.ErrInvalid, buf.Bytes())
	}
	return buf.Bytes(), nil
}

// ended makes errEnd, the body ending inside a value or an object, a 400.
func (s *scanner) ended(err error) error {
	if errors.Is(err, errEnd) {
		return fmt.Errorf("%w: the publish body ends in a value", format.ErrInvalid)
	}
	return err
}

// rest copies the rest of a string whose opening quote was read, its closing quote included.
func (s *scanner) rest(buf *bytes.Buffer) error {
	for escaped := false; ; {
		c, err := s.next()
		if err != nil {
			return s.ended(err)
		}
		buf.WriteByte(c)
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			return nil
		}
	}
}

// scalar copies the rest of a number, true, false or null: the bytes up to the next delimiter,
// which it leaves unread.
func (s *scanner) scalar(buf *bytes.Buffer) error {
	for {
		b, err := s.r.Peek(1)
		if err != nil || bytes.IndexByte([]byte(",}] \t\n\r"), b[0]) >= 0 {
			return nil // the end of the body is the caller's to find
		}
		c, err := s.next()
		if err != nil {
			return err
		}
		buf.WriteByte(c)
	}
}

// stringReader reads the contents of a JSON string whose opening quote was read, its escapes
// decoded, and io.EOF at its closing quote, which it consumes. Its bytes are not counted against
// scanner.left: they are the tarball. It copies what the buffer holds up to the next quote or
// backslash at once; an escape of a character outside ASCII, which base64 never has, is
// ErrInvalid, and a control character is left to the base64 decoder, which refuses it.
type stringReader struct {
	r    *bufio.Reader
	done bool
}

func (sr *stringReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && !sr.done {
		if _, err := sr.r.Peek(1); err != nil {
			return n, sr.cut(err)
		}
		chunk, _ := sr.r.Peek(min(len(p)-n, sr.r.Buffered()))
		plain := bytes.IndexAny(chunk, `"\`)
		if plain < 0 {
			plain = len(chunk)
		}
		n += copy(p[n:], chunk[:plain])
		_, _ = sr.r.Discard(plain)
		if plain == len(chunk) {
			continue
		}
		c, _ := sr.r.ReadByte() // the quote or the backslash Peek saw
		if c == '"' {
			sr.done = true
			break
		}
		c, err := sr.escape()
		if err != nil {
			return n, err
		}
		p[n] = c
		n++
	}
	if sr.done && n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// escape reads the escape after a backslash and returns its character.
func (sr *stringReader) escape() (byte, error) {
	e, err := sr.r.ReadByte()
	if err != nil {
		return 0, sr.cut(err)
	}
	if i := bytes.IndexByte([]byte(`"\/bfnrt`), e); i >= 0 {
		return "\"\\/\b\f\n\r\t"[i], nil
	}
	var hex [4]byte
	if e == 'u' {
		if _, err := io.ReadFull(sr.r, hex[:]); err != nil {
			return 0, sr.cut(err)
		}
		if v, err := strconv.ParseUint(string(hex[:]), 16, 16); err == nil && v < 0x80 {
			return byte(v), nil
		}
	}
	return 0, fmt.Errorf("%w: the data of the attachment has the escape \\%c%s", format.ErrInvalid, e, hex[:])
}

// cut makes the end of the body inside the data a 400.
func (sr *stringReader) cut(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: the publish body ends in the data of the attachment", format.ErrInvalid)
	}
	return bodyError(err)
}

// capped passes at most left bytes of r through, and then fails with err.
type capped struct {
	r    io.Reader
	left int64
	err  error
}

func (c *capped) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.left -= int64(n); c.left < 0 {
		return n, c.err
	}
	return n, err
}
