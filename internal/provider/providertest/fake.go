// Package providertest provides Fake, an in-memory provider.Provider for the tests of ingest,
// the facade and reindex. Non-test code does not import it; it is a normal package so that the
// tests of other packages can.
package providertest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// MaxKept is the largest blob whose bytes Fake keeps. A larger blob is hashed and counted on
// push and then discarded, so a test can push gigabytes without holding them; reading it back
// gives provider.ErrNotFound.
const MaxKept = 1 << 20

// PushRecord is one successful Push or PushReference.
type PushRecord struct {
	Name   string        // the repository, as the caller named it
	Tag    string        // the tag of a PushReference; empty for a Push
	Digest digest.Digest // what was pushed
	Size   int64
}

var (
	_ provider.Provider   = (*Fake)(nil)
	_ provider.Repository = (*repository)(nil)
)

// Fake is a provider.Provider that keeps repositories in memory: blobs and manifests by digest,
// tags, and referrers by the subject of the manifests pushed. It behaves like a registry with
// create-on-push, the referrers API, a catalog, manifest deletes and mutable tags. Push and
// PushReference read the content to its end and check its size and digest against the
// descriptor, as a registry does. It records every successful push.
//
// The Fail fields inject errors: when one is set, every call of that method returns it,
// wrapped with the operation. Set them before the calls they should affect; they are not
// guarded for changes during calls. Use a Fake as a pointer; it is safe for concurrent use.
type Fake struct {
	// RegistryName is Name; empty means "fake".
	RegistryName string
	// FailPush fails Push before it reads the content.
	FailPush error
	// FailPushReference fails PushReference before it reads the content.
	FailPushReference error
	// FailExists fails Exists.
	FailExists error

	mu             sync.Mutex
	repos          map[string]*repoData
	pushes         []PushRecord
	pushReferences []PushRecord
}

// repoData is the content of one repository.
type repoData struct {
	blobs     map[digest.Digest]blob
	manifests map[digest.Digest]manifest
	tags      map[string]digest.Digest
}

// blob is a blob's bytes; data is nil when the blob was larger than MaxKept.
type blob struct {
	size int64
	data []byte
}

// manifest is a manifest's bytes and what Referrers reports about it.
type manifest struct {
	desc    ocispec.Descriptor // media type, digest, size, artifactType, annotations
	data    []byte
	subject digest.Digest // empty when the manifest has no subject
}

// Name returns RegistryName, or "fake".
func (f *Fake) Name() string {
	if f.RegistryName == "" {
		return "fake"
	}
	return f.RegistryName
}

// Capabilities reports create-on-push, referrers, catalog and manifest deletes.
func (f *Fake) Capabilities(context.Context) (provider.Capabilities, error) {
	return provider.Capabilities{CreateOnPush: true, Referrers: true, Catalog: true, DeleteManifest: true}, nil
}

// Repository returns the repository name; it exists once something is pushed to it.
func (f *Fake) Repository(_ context.Context, name string) (provider.Repository, error) {
	if name == "" {
		return nil, errors.New("providertest: repository: empty name")
	}
	return &repository{f: f, name: name}, nil
}

// EnsureRepository does nothing: the Fake creates repositories on push.
func (f *Fake) EnsureRepository(context.Context, string) error { return nil }

// ListRepositories returns the names of the repositories that hold something and start with
// prefix, sorted.
func (f *Fake) ListRepositories(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := []string{}
	for _, name := range slices.Sorted(maps.Keys(f.repos)) {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// DeleteRepository removes the repository and its content.
func (f *Fake) DeleteRepository(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.repos[name]; !ok {
		return fmt.Errorf("providertest: delete repository %s: %w", name, provider.ErrNotFound)
	}
	delete(f.repos, name)
	return nil
}

// BlobRedirectURL answers that there is no redirect: callers stream through Fetch.
func (f *Fake) BlobRedirectURL(context.Context, string, digest.Digest) (string, bool, error) {
	return "", false, nil
}

// Pushes returns the successful Push calls so far, in order.
func (f *Fake) Pushes() []PushRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pushes)
}

// PushReferences returns the successful PushReference calls so far, in order.
func (f *Fake) PushReferences() []PushRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pushReferences)
}

// repo returns the content of name, creating it when create is set; nil when it does not exist.
// The caller holds f.mu.
func (f *Fake) repo(name string, create bool) *repoData {
	r, ok := f.repos[name]
	if !ok && create {
		if f.repos == nil {
			f.repos = map[string]*repoData{}
		}
		r = &repoData{blobs: map[digest.Digest]blob{}, manifests: map[digest.Digest]manifest{}, tags: map[string]digest.Digest{}}
		f.repos[name] = r
	}
	return r
}

// repository is one repository of a Fake.
type repository struct {
	f    *Fake
	name string
}

// op names an operation for an error message.
func (r *repository) op(verb string, ref any) string {
	return fmt.Sprintf("providertest: %s %s@%v", verb, r.name, ref)
}

// Fetch returns the manifest or blob target. A discarded blob is provider.ErrNotFound.
func (r *repository) Fetch(_ context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if rd := r.f.repo(r.name, false); rd != nil {
		if m, ok := rd.manifests[target.Digest]; ok {
			return io.NopCloser(bytes.NewReader(m.data)), nil
		}
		if b, ok := rd.blobs[target.Digest]; ok && b.data != nil {
			return io.NopCloser(bytes.NewReader(b.data)), nil
		}
		if _, ok := rd.blobs[target.Digest]; ok {
			return nil, fmt.Errorf("%s: larger than %d bytes, discarded on push: %w", r.op("fetch", target.Digest), MaxKept, provider.ErrNotFound)
		}
	}
	return nil, fmt.Errorf("%s: %w", r.op("fetch", target.Digest), provider.ErrNotFound)
}

// Exists reports whether the manifest or blob target is in the repository.
func (r *repository) Exists(_ context.Context, target ocispec.Descriptor) (bool, error) {
	if err := r.f.FailExists; err != nil {
		return false, fmt.Errorf("%s: %w", r.op("exists", target.Digest), err)
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	rd := r.f.repo(r.name, false)
	if rd == nil {
		return false, nil
	}
	_, isBlob := rd.blobs[target.Digest]
	_, isManifest := rd.manifests[target.Digest]
	return isBlob || isManifest, nil
}

// Push stores the blob or manifest expected, read from content and checked against expected.
// Content that exists already is success.
func (r *repository) Push(_ context.Context, expected ocispec.Descriptor, content io.Reader) error {
	if err := r.f.FailPush; err != nil {
		return fmt.Errorf("%s: %w", r.op("push", expected.Digest), err)
	}
	data, err := receive(expected, content)
	if err != nil {
		return fmt.Errorf("%s: %w", r.op("push", expected.Digest), err)
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.store(expected, data)
	r.f.pushes = append(r.f.pushes, PushRecord{Name: r.name, Digest: expected.Digest, Size: expected.Size})
	return nil
}

// PushReference stores the manifest expected and points reference at it, re-pointing a tag
// that exists: the Fake is a registry without immutable tags.
func (r *repository) PushReference(_ context.Context, expected ocispec.Descriptor, content io.Reader, reference string) error {
	if err := r.f.FailPushReference; err != nil {
		return fmt.Errorf("%s: %w", r.op("push", reference), err)
	}
	if !isManifest(expected.MediaType) {
		return fmt.Errorf("%s: media type %q is not a manifest's", r.op("push", reference), expected.MediaType)
	}
	data, err := receive(expected, content)
	if err != nil {
		return fmt.Errorf("%s: %w", r.op("push", reference), err)
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.store(expected, data)
	r.f.repo(r.name, true).tags[reference] = expected.Digest
	r.f.pushReferences = append(r.f.pushReferences, PushRecord{Name: r.name, Tag: reference, Digest: expected.Digest, Size: expected.Size})
	return nil
}

// store keeps a received blob or manifest; data is nil for a discarded blob. The caller holds
// r.f.mu.
func (r *repository) store(desc ocispec.Descriptor, data []byte) {
	rd := r.f.repo(r.name, true)
	if !isManifest(desc.MediaType) {
		rd.blobs[desc.Digest] = blob{size: desc.Size, data: data}
		return
	}
	var parsed struct {
		ArtifactType string              `json:"artifactType"`
		Config       ocispec.Descriptor  `json:"config"`
		Subject      *ocispec.Descriptor `json:"subject"`
		Annotations  map[string]string   `json:"annotations"`
	}
	_ = json.Unmarshal(data, &parsed) // a manifest that does not parse has no subject
	m := manifest{desc: ocispec.Descriptor{MediaType: desc.MediaType, Digest: desc.Digest, Size: desc.Size,
		ArtifactType: cmp.Or(parsed.ArtifactType, parsed.Config.MediaType), Annotations: parsed.Annotations}, data: data}
	if parsed.Subject != nil {
		m.subject = parsed.Subject.Digest
	}
	rd.manifests[desc.Digest] = m
}

// receive reads content to its end and checks its size and digest against expected. It returns
// the bytes, or nil when expected is a blob larger than MaxKept: those are only hashed.
func receive(expected ocispec.Descriptor, content io.Reader) ([]byte, error) {
	if err := expected.Digest.Validate(); err != nil {
		return nil, fmt.Errorf("descriptor: %w", err)
	}
	var kept bytes.Buffer
	sink := io.Writer(io.Discard)
	if isManifest(expected.MediaType) || expected.Size <= MaxKept {
		sink = &kept
	}
	d := expected.Digest.Algorithm().Digester()
	n, err := io.Copy(io.MultiWriter(d.Hash(), sink), content)
	if err != nil {
		return nil, fmt.Errorf("read content: %w", err)
	}
	if n != expected.Size || d.Digest() != expected.Digest {
		return nil, fmt.Errorf("content is %d bytes with digest %s, the descriptor says %d bytes with %s",
			n, d.Digest(), expected.Size, expected.Digest)
	}
	if sink == io.Discard {
		return nil, nil
	}
	return kept.Bytes(), nil
}

// Delete removes the manifest target, with the tags that point at it, or the blob target.
func (r *repository) Delete(_ context.Context, target ocispec.Descriptor) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	rd := r.f.repo(r.name, false)
	if rd == nil {
		return fmt.Errorf("%s: %w", r.op("delete", target.Digest), provider.ErrNotFound)
	}
	if _, ok := rd.manifests[target.Digest]; ok {
		delete(rd.manifests, target.Digest)
		maps.DeleteFunc(rd.tags, func(_ string, d digest.Digest) bool { return d == target.Digest })
		return nil
	}
	if _, ok := rd.blobs[target.Digest]; ok {
		delete(rd.blobs, target.Digest)
		return nil
	}
	return fmt.Errorf("%s: %w", r.op("delete", target.Digest), provider.ErrNotFound)
}

// Tag points reference at the manifest desc, which must be in the repository.
func (r *repository) Tag(_ context.Context, desc ocispec.Descriptor, reference string) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	rd := r.f.repo(r.name, false)
	if rd == nil {
		return fmt.Errorf("%s: %w", r.op("tag", reference), provider.ErrNotFound)
	}
	if _, ok := rd.manifests[desc.Digest]; !ok {
		return fmt.Errorf("%s: manifest %s: %w", r.op("tag", reference), desc.Digest, provider.ErrNotFound)
	}
	rd.tags[reference] = desc.Digest
	return nil
}

// Resolve returns the descriptor of the manifest that reference, a tag or a digest, names.
func (r *repository) Resolve(_ context.Context, reference string) (ocispec.Descriptor, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if rd := r.f.repo(r.name, false); rd != nil {
		d, ok := rd.tags[reference]
		if !ok {
			d = digest.Digest(reference)
		}
		if m, ok := rd.manifests[d]; ok {
			return m.desc, nil
		}
	}
	return ocispec.Descriptor{}, fmt.Errorf("%s: %w", r.op("resolve", reference), provider.ErrNotFound)
}

// Tags calls fn once with the tags after last, sorted; a repository without tags is
// provider.ErrNotFound, as registries answer.
func (r *repository) Tags(_ context.Context, last string, fn func(tags []string) error) error {
	r.f.mu.Lock()
	rd := r.f.repo(r.name, false)
	var tags []string
	if rd != nil {
		for _, t := range slices.Sorted(maps.Keys(rd.tags)) {
			if t > last {
				tags = append(tags, t)
			}
		}
	}
	r.f.mu.Unlock()
	if rd == nil {
		return fmt.Errorf("%s: %w", r.op("tags", ""), provider.ErrNotFound)
	}
	return fn(tags)
}

// Referrers calls fn once with the manifests whose subject is desc, of artifactType when it is
// not empty, sorted by digest.
func (r *repository) Referrers(_ context.Context, desc ocispec.Descriptor, artifactType string, fn func(referrers []ocispec.Descriptor) error) error {
	r.f.mu.Lock()
	var refs []ocispec.Descriptor
	if rd := r.f.repo(r.name, false); rd != nil {
		for _, d := range slices.Sorted(maps.Keys(rd.manifests)) {
			m := rd.manifests[d]
			if m.subject == desc.Digest && (artifactType == "" || m.desc.ArtifactType == artifactType) {
				refs = append(refs, m.desc)
			}
		}
	}
	r.f.mu.Unlock()
	return fn(refs)
}

// manifestMediaTypes are the media types the Fake stores as manifests.
var manifestMediaTypes = map[string]bool{
	ocispec.MediaTypeImageManifest:                              true,
	ocispec.MediaTypeImageIndex:                                 true,
	"application/vnd.docker.distribution.manifest.v2+json":      true,
	"application/vnd.docker.distribution.manifest.list.v2+json": true,
}

// isManifest reports whether mediaType is a manifest's.
func isManifest(mediaType string) bool { return manifestMediaTypes[mediaType] }
