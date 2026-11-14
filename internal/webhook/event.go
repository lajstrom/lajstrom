package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The types of events (docs/spec/webhooks.md §1), as a webhook's events name them. The
// CloudEvents type is typePrefix and the type.
const (
	TypeVersionPublished = "version.published"
	TypeVersionDeleted   = "version.deleted"
	TypeRepoCreated      = "repo.created"
	TypeRepoDeleted      = "repo.deleted"
	// TypePing is sent only by the test of one webhook; no webhook asks for it.
	TypePing = "webhook.ping"
)

// Types are the types a webhook may ask for, in the order of webhooks.md §1.
var Types = []string{TypeVersionPublished, TypeVersionDeleted, TypeRepoCreated, TypeRepoDeleted}

// typePrefix makes a type a CloudEvents type: the reverse domain name of lajstrom.dev.
const typePrefix = "dev.lajstrom."

// Schema is the version of the data of an event (webhooks.md §3).
const Schema = 1

// Event is what a write knows of its event: the type, the repo it is of, the subject of its
// audit row, and its data.
type Event struct {
	Type    string
	Repo    index.Repo // Format, Name and Kind are read
	Subject string     // the audit row's subject; "" for a repo
	Data    any        // VersionData, RepoData or PingData, marshalled as it is
}

// Actor is the principal of a write and a user's name at the time.
type Actor struct {
	Principal string `json:"principal"`
	Name      string `json:"name,omitempty"`
}

// RepoRef names a repo in an event.
type RepoRef struct {
	Format string `json:"format"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
}

// RefOf is the RepoRef of r.
func RefOf(r index.Repo) RepoRef { return RepoRef{Format: r.Format, Name: r.Name, Kind: r.Kind} }

// File is a file layer of a published version.
type File struct {
	Name      string        `json:"name"`
	MediaType string        `json:"mediaType"`
	Digest    digest.Digest `json:"digest"`
	Size      int64         `json:"size"`
}

// Common are the fields of every event (webhooks.md §3).
type Common struct {
	Schema    int    `json:"schema"`
	Action    string `json:"action"`
	Actor     Actor  `json:"actor"`
	Audit     int64  `json:"audit,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// CommonOf returns the common fields of the event of the audit row ev: its action, its
// principal and the principal's name, its id, and the request id of its details.
func CommonOf(ev index.AuditEvent) Common {
	c := Common{Schema: Schema, Action: ev.Action, Actor: Actor{Principal: ev.Principal, Name: ev.PrincipalName}, Audit: ev.ID}
	var details struct {
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal(ev.Details, &details) // details the writer marshalled; nil is none
	c.RequestID = details.RequestID
	return c
}

// VersionData is the data of a version.published or version.deleted.
type VersionData struct {
	Common
	Repo          RepoRef       `json:"repo"`
	Package       string        `json:"package"`
	Version       string        `json:"version"`
	Variant       string        `json:"variant"`
	Tag           string        `json:"tag"`
	Digest        digest.Digest `json:"digest"`
	ArtifactType  string        `json:"artifactType"`
	OCIRepository string        `json:"ociRepository"`
	Files         []File        `json:"files,omitempty"`
	Upstream      string        `json:"upstream,omitempty"`
}

// RepoData is the data of a repo.created or repo.deleted; Versions and Force only for a delete.
type RepoData struct {
	Common
	Repo     RepoRef `json:"repo"`
	Versions *int    `json:"versions,omitempty"`
	Force    *bool   `json:"force,omitempty"`
}

// PingData is the data of a webhook.ping.
type PingData struct {
	Common
	Webhook struct {
		ID   int64  `json:"id"`
		Mask string `json:"mask"`
	} `json:"webhook"`
}

// cloudEvent is the document of webhooks.md §2, the CloudEvents 1.0 structured mode.
type cloudEvent struct {
	SpecVersion     string    `json:"specversion"`
	ID              string    `json:"id"`
	Source          string    `json:"source"`
	Type            string    `json:"type"`
	Subject         string    `json:"subject,omitempty"`
	Time            time.Time `json:"time"`
	DataContentType string    `json:"datacontenttype"`
	Data            any       `json:"data"`
}

// Outbox writes the deliveries of events in the transaction of their audit rows
// (webhooks.md §4). A nil *Outbox writes nothing. Its methods may be called concurrently.
type Outbox struct {
	// Source is server.externalURL, the base of every event's source; "" on the dev stack.
	Source string
	// Now is the clock of an event's time and id; nil means time.Now.
	Now func() time.Time
}

// Enqueue writes on w one delivery of ev per active webhook whose mask matches ev.Repo and whose
// events include ev.Type, each with the same body and event id; nothing when none matches.
func (o *Outbox) Enqueue(ctx context.Context, w index.Writer, ev Event) error {
	if o == nil {
		return nil
	}
	hooks, err := w.ListWebhooks(ctx)
	if err != nil {
		return fmt.Errorf("webhook: the webhooks of a %s: %w", ev.Type, err)
	}
	var matched []index.Webhook
	for _, h := range hooks {
		if h.Active && Matches(h.Mask, ev.Repo.Format, ev.Repo.Name) && Wants(h.Events, ev.Type) {
			matched = append(matched, h)
		}
	}
	if len(matched) == 0 {
		return nil
	}
	at := o.now()
	id, body, err := o.document(ev, at, o.Source+"/"+ev.Repo.Format+"/"+ev.Repo.Name)
	if err != nil {
		return err
	}
	ds := make([]index.WebhookDelivery, len(matched))
	for i, h := range matched {
		ds[i] = index.WebhookDelivery{WebhookID: h.ID, EventID: id, EventType: ev.Type, Payload: body, CreatedAt: at}
	}
	if err := w.CreateWebhookDeliveries(ctx, ds); err != nil {
		return fmt.Errorf("webhook: the deliveries of a %s: %w", ev.Type, err)
	}
	return nil
}

// document returns the id and the body of ev at at, from source.
func (o *Outbox) document(ev Event, at time.Time, source string) (string, []byte, error) {
	id := index.NewUID(at)
	body, err := json.Marshal(cloudEvent{SpecVersion: "1.0", ID: id, Source: source, Type: typePrefix + ev.Type,
		Subject: ev.Subject, Time: at.UTC(), DataContentType: "application/json", Data: ev.Data})
	if err != nil {
		return "", nil, fmt.Errorf("webhook: the body of a %s: %w", ev.Type, err)
	}
	return id, body, nil
}

func (o *Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Matches reports whether the repo mask (docs/spec/rbac.md §3) matches formatName/repo. A mask
// that does not parse matches nothing.
func Matches(mask, formatName, repo string) bool {
	f, n, err := auth.ParseMask(mask)
	return err == nil && (f == "*" || f == formatName) && (n == "*" || n == repo)
}

// Wants reports whether a webhook with events gets an event of type typ: none means every type.
func Wants(events []string, typ string) bool {
	return len(events) == 0 || slices.Contains(events, typ)
}

// IsType reports whether typ is a type a webhook may ask for.
func IsType(typ string) bool { return slices.Contains(Types, typ) }

// CloudEventType is the CloudEvents type of typ.
func CloudEventType(typ string) string { return typePrefix + strings.TrimPrefix(typ, typePrefix) }
