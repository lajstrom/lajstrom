package deb

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// An upload streams into the registry (ADR 0014, docs/adr/0014-uploads-stream-into-the-registry.md).
// The OCI repository of a .deb follows from the package name in its control file, so the upload
// route reads the head first, the ar archive up to the end of the control member (readHead);
// then the whole body, the head again at its start, passes the inspector on its way into the
// registry, which checks the archive as Parse does.

// maxHead is the longest head of a .deb that parses: the global header, debian-binary of at
// most maxDebianBinary bytes and the control member of at most MaxControl, with their headers
// and padding bytes. A control member that does not fit is refused by its header, before its
// bytes are read.
const maxHead = len(arMagic) + 2*(arHeaderLen+1) + maxDebianBinary + MaxControl

var _ format.Streamer = Plugin{}

// Inspect implements format.Streamer: Build needs the md5 and sha1 of the file besides its
// sha256, the three that apt's Packages lists, and the control file, which the inspector
// reads. The suite and the component are checked before a byte streams, as Build checks them.
func (Plugin) Inspect(_ index.Repo, up format.StreamUpload) (format.Inspection, error) {
	if _, _, err := distParams(up.Params); err != nil {
		return format.Inspection{}, err
	}
	return format.Inspection{Checksums: []string{"md5", "sha1"}, Inspector: &inspector{}}, nil
}

// readHead reads the head of a .deb from body with ParseControl, which reads nothing after the
// control member, and returns the package and the bytes read: the start of the body, to stream
// again before the rest. Errors as ParseControl.
func readHead(body io.Reader) (*Package, []byte, error) {
	var head bytes.Buffer
	pkg, err := ParseControl(io.TeeReader(io.LimitReader(body, int64(maxHead)), &head), MaxControl)
	return pkg, head.Bytes(), err
}

// inspector is the format.Inspector of a .deb: it reads the archive as it streams, in one pass
// and with the checks of Parse, and holds the head until the head is whole and ParseControl
// has read it, then the header of the data member; of the data member's bytes it counts how
// many came. What follows the data member is not looked at, as deb(5) asks.
type inspector struct {
	head []byte    // the head as it arrives, until it is parsed
	pkg  *Package  // the control file, once the head is parsed
	hdr  []byte    // the header of the data member as it arrives
	data *arHeader // the data member, once its header is read
	left int64     // the bytes of the data member and its padding byte still to come
	err  error     // the first error, which every later Write returns
	done bool      // Close found the archive whole
}

// Write implements io.Writer: it fails with the error Parse would report once the bytes so far
// say what is wrong with the archive.
func (in *inspector) Write(p []byte) (int, error) {
	for rest := p; len(rest) > 0 && in.err == nil; {
		switch {
		case in.pkg == nil:
			rest = in.readHead(rest)
		case in.data == nil:
			rest = in.readDataHeader(rest)
		case in.left > 0:
			k := min(int64(len(rest)), in.left)
			in.left -= k
			rest = rest[k:]
		default:
			rest = nil
		}
	}
	if in.err != nil {
		return 0, in.err
	}
	return len(p), nil
}

// readHead appends to the head what of p belongs to it, parses the head once it is whole and
// returns the rest of p.
func (in *inspector) readHead(p []byte) []byte {
	for {
		n, known := headLen(in.head)
		k := min(len(p), n-len(in.head))
		in.head, p = append(in.head, p[:k]...), p[k:]
		switch {
		case len(in.head) < n:
			return p
		case known:
			in.pkg, in.err = ParseControl(bytes.NewReader(in.head), MaxControl)
			in.head = nil
			return p
		}
	}
}

// headLen returns the length of the head of a .deb whose first bytes are b: known is false when
// b is too short to tell, and n is then the length b must reach to tell more; it is never less
// than len(b) when b is what an earlier call asked for. A global header that is not one, a
// member header that is malformed, or one that announces a member over its bound, ends the
// head: ParseControl refuses it there, as Parse does.
func headLen(b []byte) (n int, known bool) {
	switch {
	case len(b) < len(arMagic):
		return len(arMagic), false
	case string(b[:len(arMagic)]) != arMagic:
		return len(arMagic), true
	}
	n = len(arMagic)
	for _, bound := range []int64{maxDebianBinary, MaxControl} {
		if len(b) < n+arHeaderLen {
			return n + arHeaderLen, false
		}
		h, err := parseArHeader(b[n : n+arHeaderLen])
		n += arHeaderLen
		if err != nil || h.size > bound {
			return n, true
		}
		n += int(h.size + h.size%2)
	}
	return n, true
}

// readDataHeader appends to the header of the data member what of p belongs to it, checks the
// header once it is whole as Parse checks the third member, and returns the rest of p.
func (in *inspector) readDataHeader(p []byte) []byte {
	k := min(len(p), arHeaderLen-len(in.hdr))
	in.hdr, p = append(in.hdr, p[:k]...), p[k:]
	if len(in.hdr) < arHeaderLen {
		return p
	}
	h, err := parseArHeader(in.hdr)
	switch {
	case err != nil:
		in.err = fmt.Errorf("%w: member 3: %w", format.ErrInvalid, err)
	case !isDataMember(h.name):
		in.err = fmt.Errorf("%w: the third member is %q, not data.tar with the extension of a compression dpkg reads",
			format.ErrInvalid, h.name)
	default:
		in.data, in.left = &h, h.size+h.size%2
	}
	return p
}

// Close implements format.Inspector: the archive must have ended after the data member and its
// padding byte, with the error of Parse otherwise.
func (in *inspector) Close() error {
	switch {
	case in.err != nil:
		return in.err
	case in.pkg == nil:
		return headError(in.head)
	case in.data == nil && len(in.hdr) == 0:
		return fmt.Errorf("%w: the archive ends after %s, before the data member", format.ErrInvalid, in.pkg.ControlMember)
	case in.data == nil:
		return fmt.Errorf("%w: truncated archive: the header of member 3 has %d of %d bytes", format.ErrInvalid, len(in.hdr), arHeaderLen)
	case in.left > in.data.size%2:
		return fmt.Errorf("%w: truncated archive: member 3 lacks %d bytes", format.ErrInvalid, in.left-in.data.size%2)
	case in.left > 0:
		return fmt.Errorf("%w: truncated archive: member 3 lacks its padding byte", format.ErrInvalid)
	}
	in.done = true
	return nil
}

// headError returns the error of an archive that ends in its head, b: the one Parse reports, of
// the first two members.
func headError(b []byte) error {
	ar, err := newArReader(bytes.NewReader(b))
	if err == nil {
		_, err = readControl(ar, MaxControl)
	}
	if err == nil {
		err = ar.skip()
	}
	if err == nil { // the head was whole, and Write parsed it
		err = errors.New("deb: the inspector ended in a whole head")
	}
	return err
}

// streamedPackage returns what the inspector of the streamed file f read, with the size and the
// checksums that ingest computed. A file that did not pass the deb inspector to its end, or
// lacks a size or one of the checksums, wraps format.ErrInvalid.
func streamedPackage(f format.UploadedFile) (*Package, error) {
	in, ok := f.Inspector.(*inspector)
	if !ok || !in.done {
		return nil, fmt.Errorf("%w: %q was streamed without the inspector of the deb plugin, or not to its end", format.ErrInvalid, f.Name)
	}
	pkg := *in.pkg
	pkg.Size, pkg.MD5, pkg.SHA1, pkg.SHA256 = f.Size, f.Checksums["md5"], f.Checksums["sha1"], f.Checksums["sha256"]
	if pkg.Size < 0 || pkg.MD5 == "" || pkg.SHA1 == "" || pkg.SHA256 == "" {
		return nil, fmt.Errorf("%w: %q was streamed without its size, md5, sha1 and sha256", format.ErrInvalid, f.Name)
	}
	return &pkg, nil
}
