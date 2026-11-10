package probe

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
)

const (
	// readBlobSize is the size of the blob of the redirect and range measurements.
	readBlobSize = 64 << 10
	// minBlobTry is where the largest-blob measurement stops halving.
	minBlobTry = 1 << 20
)

// blobSeen is what the blob read measurements found.
type blobSeen struct {
	get, head  answer // the first answers, redirects not followed
	final      answer // the GET after the redirect, or the first one
	ranged     answer // Range: bytes=0-9
	pastEnd    answer // a range past the end
	redirected bool
}

// measureBlobReads measures "Blob redirect" and "`Range` and `Accept-Ranges`": a blob of
// 64 KiB read without following a redirect, by GET and HEAD; then read whole, a range of ten
// bytes and a range past its end, after the redirect when there is one.
func (r *run) measureBlobReads(ctx context.Context) error {
	const repo = "blob"
	data, err := io.ReadAll(newPattern("read "+r.id, readBlobSize))
	if err != nil {
		return err
	}
	d, a, err := r.pushBlob(ctx, repo, data)
	if err != nil {
		return err
	}
	if !a.ok(http.StatusCreated) {
		return fmt.Errorf("push of a blob: %s", a.refusal())
	}
	var s blobSeen
	target := r.path(repo, "blobs/"+d.String())
	if s.get, err = r.get(ctx, target, nil); err != nil {
		return err
	}
	if s.head, err = r.c.do(ctx, request{method: http.MethodHead, target: target}); err != nil {
		return err
	}
	s.final = s.get
	read := target
	if loc := s.get.location(); loc != nil && s.get.status/100 == 3 {
		s.redirected, read = true, loc.String()
		if s.final, err = r.get(ctx, read, nil); err != nil {
			return err
		}
	}
	if s.ranged, err = r.get(ctx, read, http.Header{"Range": {"bytes=0-9"}}); err != nil {
		return err
	}
	past := fmt.Sprintf("bytes=%d-%d", readBlobSize, readBlobSize+9)
	if s.pastEnd, err = r.get(ctx, read, http.Header{"Range": {past}}); err != nil {
		return err
	}
	if s.final.status == http.StatusOK && s.final.digest != d {
		return fmt.Errorf("the blob read back has the digest %s, not %s", s.final.digest, d)
	}
	r.set("blobRedirect", redirectVerdict(r.c.base, s))
	r.set("range", rangeVerdict(s))
	return nil
}

// redirectVerdict is the cell "Blob redirect".
func redirectVerdict(base *url.URL, s blobSeen) string {
	if !s.redirected {
		if s.get.status == http.StatusOK {
			return "none: the bytes"
		}
		return "none: " + s.get.refusal()
	}
	loc := s.get.location()
	where := "the registry's own host"
	if loc.Host != base.Host {
		where = "`" + loc.Host + "`"
	}
	v := fmt.Sprintf("%d to %s", s.get.status, where)
	if sec, err := strconv.Atoi(loc.Query().Get("X-Amz-Expires")); err == nil {
		v += fmt.Sprintf(", presigned for %d min", sec/60)
	}
	if s.head.status/100 != 3 {
		v += "; not for `HEAD`"
	}
	return v
}

// rangeVerdict is the cell "`Range` and `Accept-Ranges`".
func rangeVerdict(s blobSeen) string {
	var v string
	switch {
	case s.ranged.status == http.StatusPartialContent:
		v = "206"
	case s.ranged.status == http.StatusOK:
		return "**none: 200 with the whole blob**" + acceptRanges(s)
	case s.ranged.status >= 500:
		return fmt.Sprintf("**%d to a `Range`**", s.ranged.status) + acceptRanges(s)
	default:
		v = "a range is " + s.ranged.refusal()
	}
	if s.pastEnd.status == http.StatusRequestedRangeNotSatisfiable {
		v += ", 416 past the end"
	} else {
		v += ", past the end " + strconv.Itoa(s.pastEnd.status)
	}
	if s.redirected {
		v += ", after the redirect"
	}
	return v + acceptRanges(s)
}

// acceptRanges says where the registry answers Accept-Ranges: bytes.
func acceptRanges(s blobSeen) string {
	onGet := strings.EqualFold(s.final.header.Get("Accept-Ranges"), "bytes")
	onHead := strings.EqualFold(s.head.header.Get("Accept-Ranges"), "bytes")
	switch {
	case onGet && onHead:
		return "; `Accept-Ranges: bytes`"
	case onHead:
		return "; `Accept-Ranges: bytes` on `HEAD` only, not on `GET`"
	case onGet:
		return "; `Accept-Ranges: bytes` on `GET` only, not on `HEAD`"
	}
	return "; no `Accept-Ranges`"
}

// pattern is a stream of size bytes made from a seed: a block of 64 KiB of SHA-256 output
// repeated, each copy stamped with its index, so that every seed and every size has a digest
// of its own and nothing is held in memory.
type pattern struct {
	block []byte
	size  int64
	off   int64
}

const patternBlock = 64 << 10

// newPattern returns the pattern of seed, size bytes long.
func newPattern(seed string, size int64) *pattern {
	b := make([]byte, 0, patternBlock)
	for i := 0; len(b) < patternBlock; i++ {
		h := sha256.Sum256([]byte(seed + "/" + strconv.Itoa(i)))
		b = append(b, h[:]...)
	}
	return &pattern{block: b[:patternBlock], size: size}
}

// Read implements io.Reader.
func (p *pattern) Read(b []byte) (int, error) {
	if p.off >= p.size {
		return 0, io.EOF
	}
	n := 0
	var stamp [8]byte
	for n < len(b) && p.off < p.size {
		i, at := p.off/patternBlock, p.off%patternBlock
		src := p.block[at:]
		if at < int64(len(stamp)) {
			binary.BigEndian.PutUint64(stamp[:], uint64(i)) // #nosec G115 -- a block index, never negative
			src = stamp[at:]
		}
		k := copy(b[n:], src[:min(int64(len(src)), p.size-p.off)])
		n += k
		p.off += int64(k)
	}
	return n, nil
}

// patternDigest returns the digest of the pattern of seed and size.
func patternDigest(seed string, size int64) (digest.Digest, error) {
	h := sha256.New()
	if _, err := io.Copy(h, newPattern(seed, size)); err != nil {
		return "", err
	}
	return digestOf(h), nil
}
