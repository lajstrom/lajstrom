package format

import (
	"context"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Proxier is implemented by a plugin whose format has proxy repos (ADR 0013,
// docs/spec/proxy-and-group.md §10): a format has the kind proxy when its plugin implements it,
// and creating a proxy repo of another format is refused. The core, internal/proxy, does
// everything else: the allow and deny lists, proxy_meta and its TTLs, the fetch claims, the
// upstream client, the spool and the checksum, the ingest call, the statuses.
//
// One kind names a document throughout: the proxy_meta row, the upstream request, the parse,
// the rendered document and its index_cache row, whose scope in a proxy repo is Key(name).
type Proxier interface {
	// Key returns name as proxy_meta, index_cache and the allow and deny patterns see it: npm
	// the name, pypi the PEP 503 name, maven groupId:artifactId. A name the format does not
	// allow gives an error wrapping ErrInvalid.
	Key(name string) (string, error)

	// MetadataRequest returns the upstream request for the document of kind of name.
	MetadataRequest(name, kind string) (UpstreamRequest, error)

	// ParseMetadata reads a document of kind of name as the upstream sent it, or as an earlier
	// parse kept it (UpstreamIndex.Keep, which must parse to the same Entries). A body it cannot
	// read gives an error wrapping ErrInvalid.
	ParseMetadata(name, kind string, body []byte) (*UpstreamIndex, error)

	// FileRequest maps a path below /<format>/<repo>/ to the artifact and the file it names. A
	// path that names no file of the format gives an error wrapping index.ErrNotFound.
	FileRequest(path string) (ArtifactRef, error)

	// RenderProxy renders the document of kind of name for a proxy repo (§7) from kept, what
	// proxy_meta holds of the upstream's document (nil without a row: the repo's versions
	// alone, as a hosted repo's document), and the versions of the name that st has in repo,
	// whose bytes (checksums, size) win over kept's. Every link to a file starts with base,
	// <server.externalURL>/<format>/<repo>/, so that a client never gets an upstream URL. The
	// Document's Kind is kind.
	RenderProxy(ctx context.Context, repo index.Repo, name, kind string, kept []byte, st index.Reader, base string) (Document, error)
}

// UpstreamRequest is a request for a document to an upstream.
type UpstreamRequest struct {
	// Path is relative to upstreams[].url, escaped, with its query when it has one: "left-pad",
	// "@acme%2fwidgets".
	Path string
	// Accept is the Accept header; "" for none.
	Accept string
}

// UpstreamIndex is what ParseMetadata reads in an upstream's document.
type UpstreamIndex struct {
	// Keep is what proxy_meta keeps: the body, or a form the plugin trimmed.
	Keep []byte
	// ContentType is the media type of Keep.
	ContentType string
	// Entries has one element per artifact the document lists.
	Entries []UpstreamEntry
}

// UpstreamEntry is one artifact an upstream's document lists: a version, and its variant when
// the format has one.
type UpstreamEntry struct {
	Version string
	Variant string
	// Files are what one fetch downloads (npm: the tarball), in the order Build takes them.
	Files []UpstreamFile
	// Params are the Upload.Params that Build needs for the artifact, as a client's upload
	// carries them; nil when it needs none.
	Params map[string]string
	// Published is the upstream's publish time of the version; zero when it gives none (ADR
	// 0008, quarantine).
	Published time.Time
}

// UpstreamFile is one file of an UpstreamEntry.
type UpstreamFile struct {
	// Name is the file's name: UploadedFile.Name for Build, and the name of its files row.
	Name string
	// URL is where the upstream serves it: absolute, or relative to upstreams[].url.
	URL string
	// Checksums are the upstream's checksums by algorithm, sha512, sha256 and sha1, as lowercase
	// hex. The strongest one is verified before the ingest; a file without one is refused.
	Checksums map[string]string
}

// ArtifactRef is the artifact and the file a request path names (FileRequest).
type ArtifactRef struct {
	// Name is the package name as the format spells it.
	Name string
	// Version and Variant ("" when the format has none) name the artifact.
	Version string
	Variant string
	// File is the file's name: UpstreamFile.Name and the name of its files row.
	File string
	// Kind is the document kind whose Entries list the artifact.
	Kind string
}

// Proxy is the core of proxy repos as the plugins' handlers see it; *proxy.Core implements it
// (docs/spec/proxy-and-group.md §4 to §7). Both methods take a repo of kind proxy (#311 extends
// them to groups); another kind gives an error wrapping ErrUnsupported. Their errors wrap the
// sentinels that StatusOf answers with the statuses of §6.
type Proxy interface {
	// Document returns the document of kind of name: after the allow and deny lists (§3), the
	// index_cache row of (repo, Key(name), kind) while the proxy_meta row of (Key(name), kind)
	// is fresh; otherwise the upstream is asked (§4.2), the row written and the index_cache
	// rows of scope Key(name) invalidated in one transaction, and the document rendered
	// (Proxier.RenderProxy) and stored. Without a row it is rendered and not stored.
	Document(ctx context.Context, repo index.Repo, name, kind string) (*index.Cache, error)

	// File returns the file that path names (Proxier.FileRequest), held or fetched from the
	// upstream and ingested on a miss (§5) by the principal of ctx (auth.PrincipalFrom). The
	// handler serves it as a hosted file is served (ServeBlob).
	File(ctx context.Context, repo index.Repo, path string) (*index.FileMatch, error)
}
