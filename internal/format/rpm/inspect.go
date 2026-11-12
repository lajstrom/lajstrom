package rpm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"

	"github.com/lajstrom/lajstrom/internal/format"
)

// inspector reads an rpm as it streams, in one pass and with the checks of Parse: it holds the
// head until headLen says it is whole and parseHead has read it, then counts the payload and,
// when the header gives a payload digest, hashes it. Close checks the size the signature header
// gives and the digest.
type inspector struct {
	head    []byte    // the head as it arrives, until it is parsed
	pkg     *Package  // the head, once parsed
	payload int64     // the bytes after the head
	sum     hash.Hash // the sha256 of the payload, when the header gives its digest
	err     error     // the first error, which every later Write returns
	done    bool      // Close found the file whole
}

// Write implements io.Writer: it fails with the error Parse would report once the bytes so far
// say what is wrong with the file.
func (in *inspector) Write(p []byte) (int, error) {
	if in.err != nil {
		return 0, in.err
	}
	rest := p
	if in.pkg == nil {
		rest = in.readHead(rest)
		if in.err != nil {
			return 0, in.err
		}
	}
	if in.pkg != nil && len(rest) > 0 {
		in.payload += int64(len(rest))
		if in.sum != nil {
			in.sum.Write(rest)
		}
	}
	return len(p), nil
}

// readHead appends to the head what of p belongs to it, parses the head once it is whole and
// returns the rest of p.
func (in *inspector) readHead(p []byte) []byte {
	for {
		n, known := headLen(in.head, MaxHeader)
		k := min(len(p), max(n-len(in.head), 0))
		in.head, p = append(in.head, p[:k]...), p[k:]
		switch {
		case len(in.head) < n:
			return p
		case known:
			in.pkg, in.err = parseHead(in.head, MaxHeader)
			in.head = nil
			if in.pkg != nil && in.pkg.payloadDigest != "" {
				in.sum = sha256.New()
			}
			return p
		}
	}
}

// Close implements format.Inspector: the head must be whole, the file as long as the signature
// header says (SIZE or LONGSIZE, header and payload) and the payload of the digest the header
// gives (PAYLOADDIGEST of SHA-256), when they say.
func (in *inspector) Close() error {
	if in.err != nil {
		return in.err
	}
	if in.pkg == nil {
		_, err := parseHead(in.head, MaxHeader)
		if err == nil { // the head was whole, and Write parsed it
			err = fmt.Errorf("rpm: the inspector ended in a whole head")
		}
		in.err = err
		return err
	}
	pkg := in.pkg
	if pkg.signed > 0 {
		got := pkg.HeaderEnd - pkg.HeaderStart + in.payload
		switch {
		case got < pkg.signed:
			in.err = fmt.Errorf("%w: truncated: header and payload have %d of the %d bytes the signature header says", format.ErrInvalid, got, pkg.signed)
		case got > pkg.signed:
			in.err = fmt.Errorf("%w: header and payload have %d bytes, the signature header says %d", format.ErrInvalid, got, pkg.signed)
		}
	}
	if in.err == nil && in.sum != nil {
		if got := hex.EncodeToString(in.sum.Sum(nil)); got != pkg.payloadDigest {
			in.err = fmt.Errorf("%w: the payload has the sha256 %s, the header says %s", format.ErrInvalid, got, pkg.payloadDigest)
		}
	}
	if in.err != nil {
		return in.err
	}
	in.done = true
	return nil
}
