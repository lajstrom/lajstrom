package ingest

import (
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// The events of ingest's writes (docs/spec/webhooks.md §1): written by Options.Webhooks in the
// transaction that completes the write's audit row, after it, so that the event carries the
// row's id and the principal's name.

// publishedEvent is the version.published of pub, after its audit row.
func (pub *publish) publishedEvent() webhook.Event {
	a := pub.parsed.Annotations
	files := make([]webhook.File, len(pub.parsed.Files))
	for i, l := range pub.parsed.Files {
		files[i] = webhook.File{Name: l.Title, MediaType: l.MediaType, Digest: l.Digest, Size: l.Size}
	}
	data := webhook.VersionData{Common: webhook.CommonOf(pub.ev), Repo: webhook.RefOf(pub.repo), Package: a.Name,
		Version: a.Version, Variant: a.Variant, Tag: pub.tag, Digest: pub.digest, ArtifactType: pub.manifest.ArtifactType,
		OCIRepository: pub.name, Files: files}
	if pub.origin != nil {
		data.Upstream = pub.origin.Upstream
	}
	return webhook.Event{Type: webhook.TypeVersionPublished, Repo: pub.repo, Subject: pub.ev.Subject, Data: data}
}

// deletedEvent is the version.deleted of d, a delete of a version, after its audit row.
func (d *deletion) deletedEvent() webhook.Event {
	data := webhook.VersionData{Common: webhook.CommonOf(d.ev), Repo: webhook.RefOf(d.repo), Package: d.pkg.Name,
		Version: d.version.Version, Variant: d.version.Variant, Tag: d.version.Tag, Digest: d.version.Digest,
		ArtifactType: d.version.ArtifactType, OCIRepository: d.name}
	return webhook.Event{Type: webhook.TypeVersionDeleted, Repo: d.repo, Subject: d.ev.Subject, Data: data}
}
