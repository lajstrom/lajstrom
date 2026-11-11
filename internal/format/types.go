package format

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Entry is what a plugin reads out of a manifest: the data for one row in versions and its rows
// in files (docs/spec/index-schema.md, Derived). Ingest adds the repo, tag and digest.
type Entry struct {
	// Name is the package name in the format's own spelling (annotation lajstrom.name).
	Name string
	// Version is the version exactly as the format spells it (annotation lajstrom.version).
	Version string
	// Variant is the platform discriminator (annotation lajstrom.variant); empty when none.
	Variant string
	// Attrs holds the format-specific annotations, lajstrom.<format>.*.
	Attrs map[string]string
	// Files has one element per file layer, in manifest order. The metadata layer is not a file.
	Files []FileEntry
	// Metadata is the content of the metadata layer; nil when the manifest has none (raw).
	Metadata json.RawMessage
}

// FileEntry is one file layer of a manifest (docs/spec/manifest-profile.md §2).
type FileEntry struct {
	// Name is the exact file name from org.opencontainers.image.title.
	Name string
	// MediaType is the layer media type, application/vnd.lajstrom.<format>.<role>.v1[+suffix].
	MediaType string
	// Digest is the layer digest.
	Digest digest.Digest
	// Size is the layer size in bytes.
	Size int64
	// Attrs holds the layer annotations.
	Attrs map[string]string
}

// Upload is what a protocol frontend received in one publish request: the files and the
// protocol's parameters. The principal is passed separately to Publisher.Upload.
type Upload struct {
	// Files are the uploaded files, in the order the client sent them.
	Files []UploadedFile
	// Params are protocol parameters: twine form fields, apt suite and component, the raw path.
	Params map[string]string
	// Origin is set by the proxy fetcher on an artifact it fetched from an upstream, and only
	// there (ADR 0013); nil on every upload of a client. Ingest writes the origin annotations
	// from it, leaves lajstrom.uploaded-by out and audits proxy.fetch instead of push.
	Origin *Origin
}

// Origin is where a proxied artifact came from (docs/spec/manifest-profile.md §3.1).
type Origin struct {
	// Upstream is the name of the upstream in the configuration (upstreams[].name), for the
	// audit row; never its URL.
	Upstream string
	// URL is the URL the first file was asked for, before any redirect, without userinfo,
	// query or fragment: lajstrom.origin.url.
	URL string
	// Checksum is the upstream's checksum that was verified before the ingest,
	// <algorithm>:<lowercase hex>: lajstrom.origin.checksum.
	Checksum string
}

// UploadedFile is one file of an Upload. It is re-openable: Build opens it to parse and digest
// it, and ingest opens it again to push the blob. The HTTP layer makes the request body
// re-openable (a request spool, never artifact storage) and removes it when the request ends.
//
// The file of a StreamUpload has no Open (ADR 0014): its bytes passed once, into an upload
// session of the registry, through the hashes and the Inspector, before Build. Build describes
// its layer from Checksums and Size and leaves it out of Built.Blobs; ingest closes the session
// once every check passed.
type UploadedFile struct {
	// Name is the file name as the client sent it; for maven, the path of the file below
	// /maven/<repo>/, com/example/lib-core/1.2.0/lib-core-1.2.0.jar.
	Name string
	// Size is the size in bytes, or -1 when unknown; known for a streamed file.
	Size int64
	// ContentType is the Content-Type the client sent, if any.
	ContentType string
	// Open returns a new reader positioned at the start of the file; nil for a streamed file.
	Open func() (io.ReadCloser, error)
	// Checksums are the digests of the whole file that the receiver computed once, while the
	// bytes streamed in, as lowercase hex by algorithm: the keys are md5, sha1, sha256 and
	// sha512. Nil when the receiver computed none. A file with all four and a known Size is in
	// the backend already (a publication session pushed it as it arrived, #53): Build may
	// describe its layer from them without opening it and leave it out of Built.Blobs, and
	// ingest checks that the backend has it. A streamed file has sha256 and those of
	// Inspection.Checksums.
	Checksums map[string]string
	// Inspector is the inspector of a streamed file (Streamer.Inspect), which every byte passed
	// through and which was closed after the last, for Build to read what it found; nil
	// otherwise.
	Inspector Inspector
}

// StreamUpload is a publish of one file that streams into the registry while it is read
// (ADR 0014, Publisher.UploadStream). The frontend knows the package before the first byte: raw
// from the path, npm from the URL, apt from the control file it read ahead (the head it read
// is the start of Body again).
type StreamUpload struct {
	// Name is the package name in the format's spelling: Plugin.EscapeName of it is the OCI
	// repository the bytes go to, where Build's manifest must be.
	Name string
	// File is the file: its Name, Size (-1 when unknown, otherwise what Body must hold) and
	// ContentType. Open, Checksums and Inspector are left to ingest.
	File UploadedFile
	// Body is the bytes of the file. Ingest reads it once, to its end; the frontend bounds it
	// by the repo's maxFileSize (LimitBody), whose error ingest returns as it is (413).
	Body io.Reader
	// Params are protocol parameters, as Upload.Params.
	Params map[string]string
	// Origin is set by the proxy fetcher only, as Upload.Origin.
	Origin *Origin
}

// Inspection is how ingest reads a streamed file for its plugin (Streamer.Inspect).
type Inspection struct {
	// Checksums are the checksums Build needs besides sha256, which ingest always computes: md5,
	// sha1, sha512, for the indexes of apt and maven.
	Checksums []string
	// Inspector reads the file as it passes; nil when Build needs nothing of its bytes but
	// their checksums (raw).
	Inspector Inspector
}

// Inspector reads a file on its way into the registry, for what Build needs of it: every byte
// goes through Write once, in order, and Close follows the last once the body ended. When the
// body fails, ingest closes the inspector too and ignores its error, so that it releases what it
// holds (the goroutine of ReadInspector). It holds no more of the bytes than its format's bounds
// (#243) and parses hostile input: an error of Write or Close refuses the upload, and its
// session is cancelled. Build finds it in UploadedFile.Inspector.
type Inspector interface {
	io.Writer
	// Close says the file ended: what the inspector looks for must have been found by now.
	Close() error
}

// Built is the result of Plugin.Build: a complete manifest and the blobs ingest has to push
// before the manifest.
type Built struct {
	// Manifest is the manifest to store; it satisfies docs/spec/manifest-profile.md §1.
	Manifest ocispec.Manifest
	// Blobs are the blobs the manifest references: config, file layers and metadata layer.
	// Ingest skips those the backend already has.
	Blobs []BlobSource
	// Entry is the index entry for the manifest, as Validate would return it.
	Entry Entry
	// Tag is the tag computed from version and variant (docs/spec/manifest-profile.md §4.2).
	Tag string
	// RepoPath is <format>/<repo>/<package path> without the instance prefix (§4.1).
	RepoPath string
}

// BlobSource is one blob to push together with a way to read its bytes.
type BlobSource struct {
	// Descriptor carries media type, digest and size of the blob.
	Descriptor ocispec.Descriptor
	// Open returns a new reader positioned at the start of the blob.
	Open func(ctx context.Context) (io.ReadCloser, error)
}

// BlobFetcher reads a blob of the manifest being validated, from the OCI repository the
// manifest is pushed to. Validate must close the returned reader. Read streams the whole blob,
// checked against its digest. The reader may also be an io.ReaderAt over the blob, of the size of
// its descriptor, that reads parts with range requests and never the whole (ingest's is, #161): a
// plugin that needs random access, to the directory at the end of a zip, type-asserts it and
// calls ReadAt and not Read, so that a facade push or a reindex reads kilobytes of a large file.
// The bytes of ReadAt are not checked against the digest, which only the whole blob can be.
type BlobFetcher func(ctx context.Context, d digest.Digest) (io.ReadCloser, error)

// IndexScope names the part of a repo an index document covers, in the format's own terms:
// "jammy/main/amd64" (apt suite/component/arch), "simple/requests" (pypi project),
// "com.example/lib-core" (maven groupId/artifactId). It is the scope column of index_cache.
type IndexScope string

// Document is one rendered index document, for example an apt Packages file or a pypi simple
// page. Callers cache it in index_cache.
type Document struct {
	// Kind is the index_cache kind: Packages, Release, InRelease, simple, maven-metadata.
	Kind string
	// Path is the URL path of the document relative to /<format>/<repo>/.
	Path string
	// ContentType is the Content-Type to serve the document with.
	ContentType string
	// Content is the document body.
	Content []byte
}

// Published is what Publisher.Upload reports back to the frontend.
type Published struct {
	// Digest is the manifest digest.
	Digest digest.Digest
	// Tag is the tag of the stored manifest.
	Tag string
	// RepoPath is <format>/<repo>/<package path> without the instance prefix.
	RepoPath string
	// Entry is the index entry of the stored manifest.
	Entry Entry
	// Existed is true for the idempotent repeat: the tag held a manifest with these files
	// already, nothing was written, and Digest is that manifest.
	Existed bool
}

// Deletion names what Publisher.Delete deletes: the manifest that Reference names in the OCI
// repository of the package path Path below the repo.
type Deletion struct {
	// Path is the package path below <format>/<repo>/, index.Package.NameOCI for a version.
	Path string
	// Reference is a tag or a digest of the manifest.
	Reference string
	// ClientIP is the client's address, for the audit row.
	ClientIP string
	// Condition, when not nil, makes the delete a compare-and-delete of a version: see
	// Condition.
	Condition *Condition
	// Job is the id of the batch the delete is part of (POST …/versions:delete), for the audit
	// row's details; empty outside a batch.
	Job string
}

// Condition makes the delete of a version conditional (docs/api.md, Delete a version): ingest
// checks it under the locks of the delete, which a move of a pointer to the version waits for
// (docs/spec/index-schema.md, Coordination), and deletes nothing when it fails. Reference must
// be the digest the client read. Under the locks:
//
//   - the package has no version Version with the variant Variant: index.ErrNotFound (gone);
//   - it has, with another digest than Reference (deleted and published again): a
//     *RefusedError, RefusedChanged;
//   - UnmodifiedSince is not zero and the version's publish time (versions.created_at, in whole
//     seconds) is after it: a *RefusedError, RefusedYoung;
//   - Force is false and a pointer names the version (an npm dist-tag): a *RefusedError,
//     RefusedProtected, naming the pointers.
type Condition struct {
	// Version and Variant are the version the client read; Variant is "" for a format without
	// variants.
	Version, Variant string
	// UnmodifiedSince is the cut-off: a version published after it is refused. Zero: none.
	UnmodifiedSince time.Time
	// Force deletes a version that pointers name.
	Force bool
}

// Refusal is why a conditional delete deleted nothing (Condition).
type Refusal string

// The refusals of a conditional delete; the batch delete of docs/api.md answers them as the
// status of an item.
const (
	// RefusedChanged: the version has another digest than the one the client read (412).
	RefusedChanged Refusal = "changed"
	// RefusedYoung: the version was published after the cut-off (412).
	RefusedYoung Refusal = "young"
	// RefusedProtected: a pointer names the version (409).
	RefusedProtected Refusal = "protected"
)

// Pointer names a version by its version string and can move to another: an npm dist-tag,
// Kind "dist-tag" and Name its tag. A conditional delete refuses a version a pointer names,
// unless it is forced.
type Pointer struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// PointersOf returns the pointers among the state of a package (index.Reader.ListJournalState)
// that name version: its npm dist-tags, in the order of the state, by kind and key.
func PointersOf(state []index.JournalState, version string) []Pointer {
	var out []Pointer
	for _, s := range state {
		if s.Kind == index.JournalDistTag && s.Value == version {
			out = append(out, Pointer{Kind: index.JournalDistTag, Name: s.Key})
		}
	}
	return out
}

// RefusedError is the error of a conditional delete whose Condition failed: nothing was
// deleted. It wraps ErrPrecondition (412) for RefusedChanged and RefusedYoung, and
// index.ErrConflict (409) for RefusedProtected.
type RefusedError struct {
	// Reason is the condition that failed.
	Reason Refusal
	// Subject names the version in the message: `version 1.0 of "requests" in pypi/libs`.
	Subject string
	// Digests are the digests of the version's variants now, for RefusedChanged.
	Digests []digest.Digest
	// PublishedAt is the publish time of the version's newest variant, for RefusedYoung.
	PublishedAt time.Time
	// Pointers are the pointers that name the version, for RefusedProtected, sorted.
	Pointers []Pointer
}

func (e *RefusedError) Error() string {
	switch e.Reason {
	case RefusedChanged:
		now := make([]string, len(e.Digests))
		for i, d := range e.Digests {
			now[i] = d.String()
		}
		return fmt.Sprintf("%s changed since it was read: its variants are %s now; nothing was deleted", e.Subject,
			strings.Join(now, ", "))
	case RefusedYoung:
		return fmt.Sprintf("%s was published at %s, after the cut-off; nothing was deleted", e.Subject,
			e.PublishedAt.UTC().Format(time.RFC3339))
	case RefusedProtected:
	}
	names := make([]string, len(e.Pointers))
	for i, p := range e.Pointers {
		names[i] = p.Kind + " " + p.Name
	}
	return fmt.Sprintf("%s is what %s points to; move it, or force the delete; nothing was deleted", e.Subject,
		strings.Join(names, ", "))
}

// Unwrap returns the sentinel of the status: index.ErrConflict for RefusedProtected,
// ErrPrecondition otherwise.
func (e *RefusedError) Unwrap() error {
	if e.Reason == RefusedProtected {
		return index.ErrConflict
	}
	return ErrPrecondition
}

// Deleted is what Publisher.Delete reports back.
type Deleted struct {
	// Digest is the manifest deleted.
	Digest digest.Digest
	// Version is the version the manifest was, as the index had it before the delete; nil for
	// a manifest that was no version: a referrer, a foreign manifest, any manifest of an oci
	// repo.
	Version *index.Version
	// Referrers is the number of referrers deleted before it, theirs included.
	Referrers int
}
