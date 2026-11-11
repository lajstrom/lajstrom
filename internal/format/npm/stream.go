package npm

import (
	"crypto/sha1" //nolint:gosec // npm's dist.shasum is a sha1, checked, not trusted
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

var _ format.Streamer = Plugin{}

// Inspect implements format.Streamer: the inspector of the tarball of a publish, which reads it
// as it streams into the registry (ADR 0014), as ReadTarball reads a whole one, and digests it
// with sha512 and sha1 for dist.integrity and dist.shasum.
func (Plugin) Inspect(index.Repo, format.StreamUpload) (format.Inspection, error) {
	return format.Inspection{Inspector: newTarballInspector()}, nil
}

// tarballInspector is the inspector of a tarball that streams into the registry: the tarball
// ReadTarball reads from its bytes on a goroutine of format.ReadInspector, and the size and the
// sha512 and sha1 of the bytes, which Build takes from it once it is closed.
type tarballInspector struct {
	format.Inspector
	tarball  *Tarball
	s512, s1 hash.Hash
	size     int64
}

func newTarballInspector() *tarballInspector {
	t := &tarballInspector{s512: sha512.New(), s1: sha1.New()} //nolint:gosec // see the import
	t.Inspector = format.ReadInspector(func(r io.Reader) error {
		tb, err := ReadTarball(r)
		t.tarball = tb
		return err
	})
	return t
}

// Write implements io.Writer.
func (t *tarballInspector) Write(p []byte) (int, error) {
	t.s512.Write(p)
	t.s1.Write(p)
	t.size += int64(len(p))
	return t.Inspector.Write(p)
}

// facts returns the facts of the tarball that streamed, whose sha256 is sum, once the inspector is
// closed.
func (t *tarballInspector) facts(sum digest.Digest) (*facts, error) {
	if t.tarball == nil {
		return nil, errors.New("npm: the inspector of the tarball found no package.json")
	}
	return &facts{tarball: t.tarball, digest: sum, size: t.size, integrity: "sha512-" + base64.StdEncoding.EncodeToString(t.s512.Sum(nil)),
		shasum: hex.EncodeToString(t.s1.Sum(nil))}, nil
}

// streamedFacts returns the facts of a tarball that streamed into the registry: what its
// inspector found, and the sha256 that ingest computed. Validate takes them as checked
// (format.ErrStreamed).
func streamedFacts(f format.UploadedFile) (*facts, error) {
	t, ok := f.Inspector.(*tarballInspector)
	sum := digest.NewDigestFromEncoded(digest.SHA256, f.Checksums["sha256"])
	if !ok || sum.Validate() != nil {
		return nil, fmt.Errorf("npm: %s streamed without the npm inspector or its sha256", f.Name)
	}
	return t.facts(sum)
}
