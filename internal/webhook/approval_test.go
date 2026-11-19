package webhook_test

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// newUser creates the user called name and returns its principal, user:<uid>.
func newUser(t *testing.T, st index.Store, name string) string {
	t.Helper()
	u := index.User{Name: name, Kind: auth.KindLocal}
	must(t, st.CreateUser(t.Context(), &u))
	return "user:" + u.UID
}

// approvalEvent appends the audit row of action about the request a by the principal by, and
// writes its event, in one transaction that returns fail.
func approvalEvent(t *testing.T, st index.Store, action, by string, a auth.Approval, fail error) error {
	t.Helper()
	return st.Tx(t.Context(), func(w index.Writer) error {
		ev := index.AuditEvent{Principal: by, Action: action, Subject: "approval:" + strconv.FormatInt(a.ID, 10),
			Details: json.RawMessage(`{"requestId":"req-9","credential":"password","signIn":{"at":"2026-11-14T09:59:00Z"}}`)}
		if err := w.AppendAudit(t.Context(), &ev); err != nil {
			return err
		}
		if err := outbox.ApprovalEvent(t.Context(), w, ev, a); err != nil {
			return err
		}
		return fail
	})
}

// TestApprovalEventMasks: the event of a request goes to the active webhooks of the admin, whose
// mask has a *, that cover the mask of its change and want its type: a binding's */*, whatever
// its own, a deploy token's its mask, a kind it does not know */*; never to a repo's webhook
// (docs/spec/webhooks.md §3, #1336).
func TestApprovalEventMasks(t *testing.T) {
	for _, tt := range []struct {
		name   string
		kind   string
		change string
		want   []string
	}{
		{"an admin binding", index.ApprovalBinding, `{"principal":"group:ops","role":"admin","repoMask":"raw/*"}`, []string{"*/*"}},
		{"a binding on */*", index.ApprovalBinding, `{"principal":"group:ops","role":"maintainer","repoMask":"*/*"}`, []string{"*/*"}},
		{"a deploy token over oci/*", index.ApprovalDeployToken, `{"name":"fleet","repoMask":"oci/*","scopes":["write"]}`,
			[]string{"*/*", "oci/*"}},
		{"a deploy token over oci/app", index.ApprovalDeployToken, `{"name":"fleet","repoMask":"oci/app","scopes":["write"]}`,
			[]string{"*/*", "*/app", "oci/*"}},
		{"a deploy token over no mask", index.ApprovalDeployToken, `{"name":"fleet","repoMask":"oci","scopes":["write"]}`,
			[]string{"*/*"}},
		{"a kind of a release after", "group", `{"group":"ops"}`, []string{"*/*"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := newStore(t)
			m := newMaster(t)
			var hooks []index.Webhook
			for _, mask := range []string{"*/*", "oci/*", "oci/app", "*/app", "raw/*"} {
				h, _ := hook(t, st, m, mask, "https://a.example.com/")
				hooks = append(hooks, h)
			}
			off, _ := hook(t, st, m, "*/*", "https://b.example.com/")
			off.Active = false
			must(t, st.UpdateWebhook(t.Context(), &off))
			publishes, _ := hook(t, st, m, "*/*", "https://c.example.com/", webhook.TypeVersionPublished)
			requests, _ := hook(t, st, m, "*/*", "https://d.example.com/", webhook.TypeApprovalRequested)
			a := auth.Approval{ID: 7, Kind: tt.kind, Change: json.RawMessage(tt.change), State: index.ApprovalPending,
				RequestedBy: "user:alice"}
			must(t, approvalEvent(t, st, auth.ActionApprovalRequest, "user:alice", a, nil))
			var got []string
			for _, h := range hooks {
				if ds := deliveries(t, st, h); len(ds) == 1 && ds[0].EventType == webhook.TypeApprovalRequested {
					got = append(got, h.Mask)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("the webhooks that got it: %v, want %v", got, tt.want)
			}
			if len(deliveries(t, st, off)) != 0 || len(deliveries(t, st, publishes)) != 0 || len(deliveries(t, st, requests)) != 1 {
				t.Error("an inactive webhook, or one of other types, got it; or one of its type did not")
			}
		})
	}
}

// approvalDoc is the document of a request's event as the tests read it.
type approvalDoc struct {
	Source, Type, Subject string
	Data                  struct {
		webhook.Common
		Approval map[string]any    `json:"approval"`
		Names    map[string]string `json:"names"`
	}
}

func approvalDocument(t *testing.T, payload []byte) approvalDoc {
	t.Helper()
	var doc approvalDoc
	must(t, json.Unmarshal(payload, &doc))
	return doc
}

// TestApprovalEventDocument: the event of a request has the source of the requests, its subject
// and type, the request as the API answers it with the fields of its change's kind only, and
// the names of its users at the time; never a deploy token's hash, the credential or the sign-in
// of either side, nor the details of its audit row.
func TestApprovalEventDocument(t *testing.T) {
	st := newStore(t)
	h, _ := hook(t, st, newMaster(t), "raw/*", "https://a.example.com/")
	alice, root := newUser(t, st, "alice"), newUser(t, st, "root")
	decided := t0.Add(-time.Minute)
	tok := auth.Approval{ID: 7, Kind: index.ApprovalDeployToken, State: index.ApprovalApproved, RequestedBy: alice, RequestedAt: t0,
		ExpiresAt: t0.Add(7 * 24 * time.Hour), DecidedBy: root, DecidedAt: &decided, DecisionReason: "CHG-2", Result: "token:3",
		ChangeReason: "CHG-1", Change: json.RawMessage(`{"name":"fleet","repoMask":"raw/*","scopes":["read","write"],` +
			`"expiresAt":null,"hash":"sha256:5ec3e7","createdBy":"password","signIn":{"at":"2026-11-14T09:59:00Z"}}`)}
	must(t, approvalEvent(t, st, auth.ActionApprovalApprove, root, tok, nil))
	ds := deliveries(t, st, h)
	if len(ds) != 1 {
		t.Fatalf("the webhook of raw/* got %d events, want one", len(ds))
	}
	payload := string(ds[0].Payload)
	for _, never := range []string{"5ec3e7", "createdBy", "signIn", "credential", "password", "details"} {
		if strings.Contains(payload, never) {
			t.Errorf("the event holds %q: %s", never, payload)
		}
	}
	doc := approvalDocument(t, ds[0].Payload)
	if doc.Source != "https://art.example.com/api/v1/approvals" || doc.Type != "dev.lajstrom.approval.approved" ||
		doc.Subject != "approval:7" || doc.Data.Action != auth.ActionApprovalApprove || doc.Data.Actor.Principal != root ||
		doc.Data.Actor.Name != "root" || doc.Data.RequestID != "req-9" || doc.Data.Audit == 0 {
		t.Errorf("the event: %s", payload)
	}
	change, _ := doc.Data.Approval["change"].(map[string]any)
	if keys := slices.Sorted(maps.Keys(change)); !slices.Equal(keys, []string{"expiresAt", "name", "repoMask", "scopes"}) {
		t.Errorf("the change has %v", keys)
	}
	want := []string{"change", "changeReason", "decidedAt", "decidedBy", "decisionReason", "expiresAt", "id", "kind", "requestedAt",
		"requestedBy", "result", "state"}
	if keys := slices.Sorted(maps.Keys(doc.Data.Approval)); !slices.Equal(keys, want) {
		t.Errorf("the request has %v, want %v", keys, want)
	}
	if want := map[string]string{alice: "alice", root: "root"}; !maps.Equal(doc.Data.Names, want) {
		t.Errorf("names %v, want %v", doc.Data.Names, want)
	}
}

// TestApprovalEventNames: names has the name of a binding's principal too, a deleted user's the
// name it had, and none for anonymous, the decider of an expiry.
func TestApprovalEventNames(t *testing.T) {
	st := newStore(t)
	h, _ := hook(t, st, newMaster(t), "*/*", "https://a.example.com/")
	alice, carol := newUser(t, st, "alice"), newUser(t, st, "carol")
	u, err := st.GetUserByUID(t.Context(), strings.TrimPrefix(carol, "user:"))
	must(t, err)
	u.Name, u.DeletedName, u.DeletedAt = "~"+u.UID, "carol", &t0
	must(t, st.UpdateUser(t.Context(), u))
	bind := auth.Approval{ID: 8, Kind: index.ApprovalBinding, State: index.ApprovalExpired, RequestedBy: alice, DecidedBy: "anonymous",
		Change: json.RawMessage(`{"principal":"` + carol + `","role":"admin","repoMask":"*/*"}`)}
	must(t, approvalEvent(t, st, auth.ActionApprovalExpire, "anonymous", bind, nil))
	ds := deliveries(t, st, h)
	if len(ds) != 1 {
		t.Fatalf("the webhook of */* got %d events, want one", len(ds))
	}
	doc := approvalDocument(t, ds[0].Payload)
	if want := map[string]string{alice: "alice", carol: "carol"}; !maps.Equal(doc.Data.Names, want) ||
		doc.Type != "dev.lajstrom.approval.expired" || doc.Subject != "approval:8" || doc.Data.Actor.Principal != "anonymous" {
		t.Errorf("the expiry's event: %s", ds[0].Payload)
	}
}

// TestApprovalEventRollsBack: the event of a request goes with its audit row's transaction:
// nothing when it rolls back. An action of no request is an error; a nil outbox writes nothing.
func TestApprovalEventRollsBack(t *testing.T) {
	st := newStore(t)
	h, _ := hook(t, st, newMaster(t), "*/*", "https://a.example.com/")
	a := auth.Approval{ID: 7, Kind: index.ApprovalBinding, Change: json.RawMessage(`{"principal":"group:ops","role":"admin","repoMask":"*/*"}`)}
	boom := errors.New("boom")
	if err := approvalEvent(t, st, auth.ActionApprovalReject, "user:alice", a, boom); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err := approvalEvent(t, st, "binding.create", "user:alice", a, nil); err == nil || !strings.Contains(err.Error(), "binding.create") {
		t.Errorf("an action of no request: %v", err)
	}
	if ds := deliveries(t, st, h); len(ds) != 0 {
		t.Errorf("%d deliveries of a transaction rolled back", len(ds))
	}
	var none *webhook.Outbox
	must(t, none.ApprovalEvent(t.Context(), st, index.AuditEvent{Action: "anything"}, a))
}
