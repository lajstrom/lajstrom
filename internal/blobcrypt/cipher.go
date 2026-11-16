package blobcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/opencontainers/go-digest"
)

// ErrMismatch is the error at the end of a blob whose ciphertext does not have its HMAC, or
// whose plaintext does not have its digest: the blob was changed in the registry, or the options
// are another blob's.
var ErrMismatch = errors.New("blobcrypt: the blob does not have its HMAC or its digest")

// At returns the keystream of the blob of p from its byte offset: AES-256 in counter mode, the
// counter the nonce plus offset/16 as a 128-bit big-endian number, as crypto/cipher's CTR
// counts, and offset%16 bytes of it passed. At(p, 0) is ocicrypt's stream.
func At(p Private, offset int64) (cipher.Stream, error) {
	if err := p.Check(); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, fmt.Errorf("blobcrypt: a negative offset, %d", offset)
	}
	block, err := aes.NewCipher(p.Key)
	if err != nil {
		return nil, fmt.Errorf("blobcrypt: %w", err)
	}
	iv := make([]byte, aes.BlockSize)
	copy(iv, p.Nonce())
	addCounter(iv, uint64(offset/aes.BlockSize))
	s := cipher.NewCTR(block, iv)
	if skip := offset % aes.BlockSize; skip > 0 {
		var b [aes.BlockSize]byte
		s.XORKeyStream(b[:skip], b[:skip])
	}
	return s, nil
}

// addCounter adds n to the 128-bit big-endian counter block iv.
func addCounter(iv []byte, n uint64) {
	hi, lo := binary.BigEndian.Uint64(iv[:8]), binary.BigEndian.Uint64(iv[8:])
	sum := lo + n
	if sum < lo {
		hi++
	}
	binary.BigEndian.PutUint64(iv[:8], hi)
	binary.BigEndian.PutUint64(iv[8:], sum)
}

// NewMAC returns the HMAC of the ciphertext of p's blob: HMAC-SHA256 under its data key.
func NewMAC(p Private) hash.Hash { return hmac.New(sha256.New, p.Key) }

// EncryptReader returns a reader of the ciphertext of the plaintext that r reads, through the
// keystream s; each byte of ciphertext is written to every writer of to as well, a hash of
// the ciphertext, its HMAC. It reads no further than the caller asks and buffers nothing.
func EncryptReader(r io.Reader, s cipher.Stream, to ...io.Writer) io.Reader {
	return &xorReader{r: r, s: s, after: io.MultiWriter(to...)}
}

// DecryptReader returns a reader of the plaintext of the ciphertext that r reads, through the
// keystream s; each byte of ciphertext is written to every writer of from as well, before it is
// decrypted: the HMAC that proves it.
func DecryptReader(r io.Reader, s cipher.Stream, from ...io.Writer) io.Reader {
	return &xorReader{r: r, s: s, before: io.MultiWriter(from...)}
}

// xorReader passes what r reads through the keystream s, in place in the caller's buffer, with
// the bytes before (ciphertext being decrypted) or after (ciphertext made) written to a hash.
type xorReader struct {
	r      io.Reader
	s      cipher.Stream
	before io.Writer
	after  io.Writer
}

func (x *xorReader) Read(p []byte) (int, error) {
	n, err := x.r.Read(p)
	if n > 0 {
		if x.before != nil {
			_, _ = x.before.Write(p[:n]) // hashes never fail
		}
		x.s.XORKeyStream(p[:n], p[:n])
		if x.after != nil {
			_, _ = x.after.Write(p[:n])
		}
	}
	return n, err
}

// Open returns the plaintext of the whole blob of p whose ciphertext r reads and whose public
// options carry mac: decrypted as it is read, and checked at its end against mac and, when
// p.Digest is set, the digest of the plaintext. A blob that does not match ends in ErrMismatch
// instead of io.EOF. The reader holds the last byte it read back until the blob ended and
// matched, so a caller that writes only what it got never writes the whole of a blob that was
// changed (docs/spec/guarantees.md, INV-40).
func Open(r io.Reader, p Private, mac []byte) (io.Reader, error) {
	s, err := At(p, 0)
	if err != nil {
		return nil, err
	}
	o := &opened{mac: NewMAC(p), want: mac, buf: make([]byte, 32<<10)}
	if p.Digest != "" {
		if err := p.Digest.Validate(); err != nil {
			return nil, fmt.Errorf("%w: the plaintext digest %q: %w", ErrInvalid, p.Digest, err)
		}
		o.plain, o.digest = p.Digest.Algorithm().Hash(), p.Digest
	}
	o.r = DecryptReader(r, s, o.mac)
	return o, nil
}

// opened is the reader of Open: held is the plaintext read and not yet returned, of which the
// last byte stays until the blob ended.
type opened struct {
	r      io.Reader
	mac    hash.Hash
	want   []byte
	plain  hash.Hash
	digest digest.Digest
	buf    []byte
	held   []byte
	done   bool
	err    error
}

func (o *opened) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		switch {
		case o.done && o.err != nil:
			return 0, o.err
		case o.done && len(o.held) == 0:
			return 0, io.EOF
		case o.done:
			n := copy(p, o.held)
			o.held = o.held[n:]
			return n, nil
		case len(o.held) > 1:
			n := copy(p, o.held[:len(o.held)-1])
			o.held = o.held[n:]
			return n, nil
		}
		k := copy(o.buf, o.held) // at most the one byte held back
		n, err := o.r.Read(o.buf[k:])
		if o.plain != nil {
			_, _ = o.plain.Write(o.buf[k : k+n])
		}
		o.held = o.buf[:k+n]
		switch {
		case errors.Is(err, io.EOF):
			o.done, o.err = true, o.check()
		case err != nil:
			return 0, err
		}
	}
}

// check is nil when the blob matched, ErrMismatch otherwise.
func (o *opened) check() error {
	if !hmac.Equal(o.mac.Sum(nil), o.want) {
		return fmt.Errorf("%w: the HMAC of the ciphertext differs", ErrMismatch)
	}
	if o.plain != nil && digest.NewDigest(o.digest.Algorithm(), o.plain) != o.digest {
		return fmt.Errorf("%w: the plaintext is not %s", ErrMismatch, o.digest)
	}
	return nil
}

// Seal returns the ciphertext of plain under p, with its HMAC: for a blob held in memory, a
// metadata layer.
func Seal(p Private, plain []byte) (ciphertext, mac []byte, err error) {
	s, err := At(p, 0)
	if err != nil {
		return nil, nil, err
	}
	ciphertext = make([]byte, len(plain))
	s.XORKeyStream(ciphertext, plain)
	m := NewMAC(p)
	_, _ = m.Write(ciphertext)
	return ciphertext, m.Sum(nil), nil
}
