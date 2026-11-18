package snapshot

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Version is the version of the document this binary writes, and the newest it reads
// (docs/spec/manifest-profile.md §9, Versions).
const Version = 1

// maxDocument bounds a document unpacked: a layer that inflates past it is refused, as a bomb.
const maxDocument = 1 << 30

// document is the JSON of a snapshot (docs/spec/manifest-profile.md §9): a row of each table
// per object, with its id; an absent key is the column's NULL or empty value.
type document struct {
	Version        int                `json:"version"`
	Schema         int                `json:"schema"`
	CreatedAt      time.Time          `json:"createdAt"`
	Binary         string             `json:"binary,omitempty"`
	AuditSince     *time.Time         `json:"auditSince,omitempty"`
	Repos          []repoRow          `json:"repos"`
	GroupMembers   []groupMemberRow   `json:"groupMembers"`
	Users          []userRow          `json:"users"`
	Identities     []identityRow      `json:"identities"`
	IdentityGroups []identityGroupRow `json:"identityGroups"`
	Tokens         []tokenRow         `json:"tokens"`
	Bindings       []bindingRow       `json:"bindings"`
	Keys           []keyRow           `json:"keys"`
	// Webhooks is absent from a document written before migration 0024: no webhook.
	Webhooks []webhookRow `json:"webhooks"`
	// PolicyRules is absent from a document written before migration 0030: no rule (#101).
	PolicyRules []policyRuleRow `json:"policyRules"`
	// CIPolicies is absent from a document written before migration 0033: no policy of the API.
	CIPolicies []ciPolicyRow `json:"ciPolicies"`
	// SealingKey is the row of sealing_key, the id of the master key that sealed then (#1288),
	// never a key; absent when the database had none, and before migration 0040.
	SealingKey *sealingKeyRow `json:"sealingKey,omitempty"`
	// Audit is nil when the snapshot was made without audit rows, and absent then.
	Audit *[]auditRow `json:"audit,omitempty"`
}

// sealingKeyRow is the row of sealing_key (#1288).
type sealingKeyRow struct {
	KeyID   string    `json:"keyId"`
	Source  string    `json:"source"`
	Host    string    `json:"host"`
	AuditID *int64    `json:"auditId,omitempty"`
	SetAt   time.Time `json:"setAt"`
}

// sealingKeyRowOf is the sealingKey of the document of k; nil for none.
func sealingKeyRowOf(k *index.SealingKey) *sealingKeyRow {
	if k == nil {
		return nil
	}
	return &sealingKeyRow{KeyID: k.KeyID, Source: k.Source, Host: k.Host, AuditID: k.AuditID, SetAt: k.SetAt}
}

// row is the row of sealing_key of r; nil for none.
func (r *sealingKeyRow) row() *index.SealingKey {
	if r == nil {
		return nil
	}
	return &index.SealingKey{KeyID: r.KeyID, Source: r.Source, Host: r.Host, AuditID: r.AuditID, SetAt: r.SetAt}
}

// ciPolicyRow is a row of ci_policies; claims and grants are the JSON of their columns.
type ciPolicyRow struct {
	ID               int64               `json:"id"`
	Name             string              `json:"name"`
	Issuer           string              `json:"issuer"`
	Claims           map[string][]string `json:"claims"`
	Grants           []index.CIGrant     `json:"grants"`
	State            string              `json:"state"`
	ExpiresAt        *time.Time          `json:"expiresAt,omitempty"`
	RenewalExpiresAt *time.Time          `json:"renewalExpiresAt,omitempty"`
	CreatedBy        string              `json:"createdBy"`
	CreatedAt        time.Time           `json:"createdAt"`
	UpdatedBy        string              `json:"updatedBy"`
	UpdatedAt        time.Time           `json:"updatedAt"`
	ApprovedBy       string              `json:"approvedBy,omitempty"`
	ApprovedAt       *time.Time          `json:"approvedAt,omitempty"`
	Revision         int64               `json:"revision"`
}

type repoRow struct {
	ID            int64           `json:"id"`
	Format        string          `json:"format"`
	Name          string          `json:"name"`
	Registry      string          `json:"registry,omitempty"`
	Prefix        string          `json:"prefix,omitempty"`
	Kind          string          `json:"kind,omitempty"`
	AnonymousRead bool            `json:"anonymousRead,omitempty"`
	Policy        json.RawMessage `json:"policy,omitempty"`
	Upstream      json.RawMessage `json:"upstream,omitempty"`
	Group         json.RawMessage `json:"group,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
	Revision      int64           `json:"revision,omitempty"` // absent before migration 0032: 1
}

type groupMemberRow struct {
	Group    int64 `json:"group"`
	Member   int64 `json:"member"`
	Position int   `json:"position"`
}

type userRow struct {
	ID           int64      `json:"id"`
	UID          string     `json:"uid"`
	Name         string     `json:"name"`
	PasswordHash string     `json:"passwordHash,omitempty"`
	Kind         string     `json:"kind,omitempty"`
	Disabled     bool       `json:"disabled,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	DeletedAt    *time.Time `json:"deletedAt,omitempty"`
	DeletedName  string     `json:"deletedName,omitempty"`
	Owner        string     `json:"owner,omitempty"`
	Revision     int64      `json:"revision,omitempty"`
	// LastSeenAt is the user's last activity; absent without one, and in a document written
	// before migration 0034, which the restore fills as that migration does.
	LastSeenAt *time.Time `json:"lastSeenAt,omitempty"`
	// MustChange is true while the password must be changed, absent otherwise; PasswordHistory
	// the argon2id hashes of the three passwords before the current one, the latest first,
	// absent without any and in a document written before migration 0037, which the restore
	// takes for none (docs/spec/index-schema.md, A password's history).
	MustChange      bool     `json:"mustChange,omitempty"`
	PasswordHistory []string `json:"passwordHistory,omitempty"`
}

type identityRow struct {
	ID             int64      `json:"id"`
	User           int64      `json:"user"`
	Issuer         string     `json:"issuer"`
	Subject        string     `json:"subject"`
	CreatedAt      time.Time  `json:"createdAt"`
	LoginAt        time.Time  `json:"loginAt"`
	GroupsExpireAt *time.Time `json:"groupsExpireAt,omitempty"`
}

type identityGroupRow struct {
	Identity int64  `json:"identity"`
	Name     string `json:"name"`
}

type tokenRow struct {
	ID         int64      `json:"id"`
	Kind       string     `json:"kind"`
	User       *int64     `json:"user,omitempty"`
	Repo       *int64     `json:"repo,omitempty"`
	RepoMask   string     `json:"repoMask,omitempty"` // a deploy token over a mask's
	Name       string     `json:"name,omitempty"`
	Hash       string     `json:"hash"`
	Scopes     []string   `json:"scopes"` // null and [] apart, as the table has them
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	DPoPJKT    string     `json:"dpopJkt,omitempty"` // a web UI's token's
	// AuthTime, AMR and ACR are those of the OIDC login of a web UI's token; absent for the
	// others, and in a document written before migrations 0033 and 0038.
	AuthTime *time.Time `json:"authTime,omitempty"`
	AMR      []string   `json:"amr,omitempty"`
	ACR      string     `json:"acr,omitempty"`
}

type bindingRow struct {
	ID        int64     `json:"id"`
	Principal string    `json:"principal"`
	Role      string    `json:"role"`
	RepoMask  string    `json:"repoMask"`
	CreatedAt time.Time `json:"createdAt"`
}

type keyRow struct {
	ID          int64      `json:"id"`
	Repo        *int64     `json:"repo,omitempty"`
	Kind        string     `json:"kind"`
	Public      []byte     `json:"public"`
	PrivateEnc  []byte     `json:"privateEnc"`
	Fingerprint string     `json:"fingerprint"`
	CreatedAt   time.Time  `json:"createdAt"`
	RetiredAt   *time.Time `json:"retiredAt,omitempty"`
}

type webhookRow struct {
	ID          int64     `json:"id"`
	UID         string    `json:"uid"`
	Mask        string    `json:"mask"`
	URL         string    `json:"url"`
	Events      []string  `json:"events,omitempty"` // absent: every type
	SecretEnc   []byte    `json:"secretEnc"`
	Active      bool      `json:"active,omitempty"`
	Description string    `json:"description,omitempty"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Revision    int64     `json:"revision,omitempty"`
}

type policyRuleRow struct {
	ID        int64      `json:"id"`
	RepoMask  string     `json:"repoMask"`
	Name      string     `json:"name"`
	Version   string     `json:"version"`
	Variant   string     `json:"variant"`
	Effect    string     `json:"effect"`
	Reason    string     `json:"reason"`
	CreatedBy string     `json:"createdBy"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

type auditRow struct {
	ID            int64           `json:"id"`
	At            time.Time       `json:"at"`
	Principal     string          `json:"principal"`
	PrincipalName string          `json:"principalName,omitempty"`
	Action        string          `json:"action"`
	Repo          string          `json:"repo,omitempty"`
	Subject       string          `json:"subject,omitempty"`
	Digest        digest.Digest   `json:"digest,omitempty"`
	IP            string          `json:"ip,omitempty"`
	Details       json.RawMessage `json:"details,omitempty"`
	State         string          `json:"state,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// encode returns the document of cp, written by binary, gzip'd: the bytes of an unsealed
// snapshot's layer, and what a sealed one encrypts.
func encode(cp *index.ControlPlane, binary string) ([]byte, error) {
	doc := documentOf(cp, binary)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(doc); err != nil {
		return nil, fmt.Errorf("snapshot: encode the document: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("snapshot: compress the document: %w", err)
	}
	return buf.Bytes(), nil
}

// ErrVersion means a snapshot's document is of a version this binary does not read.
var ErrVersion = errors.New("snapshot: a document of a version this binary does not read")

// decode reads the gzip'd document gz: at most maxDocument bytes unpacked, of a version this
// binary reads. The ControlPlane has its rows in the binary's newest form.
func decode(gz []byte) (*index.ControlPlane, string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, "", fmt.Errorf("snapshot: the document is not gzip: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, maxDocument+1))
	if err != nil {
		return nil, "", fmt.Errorf("snapshot: unpack the document: %w", err)
	}
	if len(raw) > maxDocument {
		return nil, "", fmt.Errorf("snapshot: the document is larger than %d bytes unpacked", maxDocument)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", fmt.Errorf("snapshot: the document is not JSON of a snapshot: %w", err)
	}
	if doc.Version < 1 || doc.Version > Version {
		return nil, "", fmt.Errorf("%w: version %d, this binary reads 1 to %d", ErrVersion, doc.Version, Version)
	}
	return controlPlaneOf(&doc), doc.Binary, nil
}

// documentOf is the document of cp.
func documentOf(cp *index.ControlPlane, binary string) *document {
	doc := &document{Version: Version, Schema: cp.Schema, CreatedAt: cp.ReadAt.UTC(), Binary: binary,
		Repos: make([]repoRow, 0, len(cp.Repos)), GroupMembers: make([]groupMemberRow, 0, len(cp.GroupMembers)),
		Users: make([]userRow, 0, len(cp.Users)), Identities: make([]identityRow, 0, len(cp.Identities)),
		IdentityGroups: make([]identityGroupRow, 0, len(cp.IdentityGroups)), Tokens: make([]tokenRow, 0, len(cp.Tokens)),
		Bindings: make([]bindingRow, 0, len(cp.Bindings)), Keys: make([]keyRow, 0, len(cp.Keys)),
		Webhooks: make([]webhookRow, 0, len(cp.Webhooks)), PolicyRules: make([]policyRuleRow, 0, len(cp.PolicyRules)),
		CIPolicies: make([]ciPolicyRow, 0, len(cp.CIPolicies))}
	for _, r := range cp.Repos {
		doc.Repos = append(doc.Repos, repoRow{ID: r.ID, Format: r.Format, Name: r.Name, Registry: r.Registry, Prefix: r.Prefix,
			Kind: r.Kind, AnonymousRead: r.AnonymousRead, Policy: r.Policy, Upstream: r.Upstream, Group: r.Group,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Revision: r.Revision})
	}
	for _, g := range cp.GroupMembers {
		doc.GroupMembers = append(doc.GroupMembers, groupMemberRow{Group: g.GroupID, Member: g.MemberID, Position: g.Position})
	}
	for _, u := range cp.Users {
		doc.Users = append(doc.Users, userRow{ID: u.ID, UID: u.UID, Name: u.Name, PasswordHash: u.PasswordHash, Kind: u.Kind,
			Disabled: u.Disabled, CreatedAt: u.CreatedAt, DeletedAt: u.DeletedAt, DeletedName: u.DeletedName, Owner: u.Owner,
			Revision: u.Revision, LastSeenAt: u.LastSeenAt, MustChange: u.MustChange, PasswordHistory: u.PasswordHistory})
	}
	for _, i := range cp.Identities {
		doc.Identities = append(doc.Identities, identityRow{ID: i.ID, User: i.UserID, Issuer: i.Issuer, Subject: i.Subject,
			CreatedAt: i.CreatedAt, LoginAt: i.LoginAt, GroupsExpireAt: i.GroupsExpireAt})
	}
	for _, g := range cp.IdentityGroups {
		doc.IdentityGroups = append(doc.IdentityGroups, identityGroupRow{Identity: g.IdentityID, Name: g.Name})
	}
	for _, t := range cp.Tokens {
		doc.Tokens = append(doc.Tokens, tokenRow{ID: t.ID, Kind: t.Kind, User: t.UserID, Repo: t.RepoID, RepoMask: t.RepoMask,
			Name: t.Name, Hash: t.Hash, Scopes: t.Scopes, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, CreatedAt: t.CreatedAt,
			RevokedAt: t.RevokedAt, DPoPJKT: t.DPoPJKT, AuthTime: t.AuthTime, AMR: t.AMR, ACR: t.ACR})
	}
	for _, b := range cp.Bindings {
		doc.Bindings = append(doc.Bindings, bindingRow{ID: b.ID, Principal: b.Principal, Role: b.Role, RepoMask: b.RepoMask,
			CreatedAt: b.CreatedAt})
	}
	for _, k := range cp.Keys {
		doc.Keys = append(doc.Keys, keyRow{ID: k.ID, Repo: k.RepoID, Kind: k.Kind, Public: k.Public, PrivateEnc: k.PrivateEnc,
			Fingerprint: k.Fingerprint, CreatedAt: k.CreatedAt, RetiredAt: k.RetiredAt})
	}
	for _, h := range cp.Webhooks {
		doc.Webhooks = append(doc.Webhooks, webhookRow{ID: h.ID, UID: h.UID, Mask: h.Mask, URL: h.URL, Events: h.Events,
			SecretEnc: h.SecretEnc, Active: h.Active, Description: h.Description, CreatedBy: h.CreatedBy, CreatedAt: h.CreatedAt,
			UpdatedAt: h.UpdatedAt, Revision: h.Revision})
	}
	for _, r := range cp.PolicyRules {
		doc.PolicyRules = append(doc.PolicyRules, policyRuleRow{ID: r.ID, RepoMask: r.RepoMask, Name: r.Name, Version: r.Version,
			Variant: r.Variant, Effect: r.Effect, Reason: r.Reason, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt})
	}
	for _, p := range cp.CIPolicies {
		doc.CIPolicies = append(doc.CIPolicies, ciPolicyRow{ID: p.ID, Name: p.Name, Issuer: p.Issuer, Claims: p.Claims, Grants: p.Grants,
			State: p.State, ExpiresAt: p.ExpiresAt, RenewalExpiresAt: p.RenewalExpiresAt, CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt,
			UpdatedBy: p.UpdatedBy, UpdatedAt: p.UpdatedAt, ApprovedBy: p.ApprovedBy, ApprovedAt: p.ApprovedAt, Revision: p.Revision})
	}
	doc.SealingKey = sealingKeyRowOf(cp.SealingKey)
	if cp.WithAudit {
		rows := make([]auditRow, 0, len(cp.Audit))
		for _, e := range cp.Audit {
			rows = append(rows, auditRow{ID: e.ID, At: e.At, Principal: e.Principal, PrincipalName: e.PrincipalName, Action: e.Action,
				Repo: e.Repo, Subject: e.Subject, Digest: e.Digest, IP: e.IP, Details: e.Details, State: e.State, Error: e.Error})
		}
		doc.Audit = &rows
		if !cp.AuditSince.IsZero() {
			since := cp.AuditSince.UTC()
			doc.AuditSince = &since
		}
	}
	return doc
}

// controlPlaneOf is the ControlPlane of doc.
func controlPlaneOf(doc *document) *index.ControlPlane {
	cp := &index.ControlPlane{Schema: doc.Schema, ReadAt: doc.CreatedAt, Repos: make([]index.Repo, 0, len(doc.Repos)),
		GroupMembers: make([]index.GroupMember, 0, len(doc.GroupMembers)), Users: make([]index.User, 0, len(doc.Users)),
		Identities: make([]index.Identity, 0, len(doc.Identities)), IdentityGroups: make([]index.IdentityGroup, 0, len(doc.IdentityGroups)),
		Tokens: make([]index.Token, 0, len(doc.Tokens)), Bindings: make([]index.Binding, 0, len(doc.Bindings)),
		Keys: make([]index.Key, 0, len(doc.Keys)), Webhooks: make([]index.Webhook, 0, len(doc.Webhooks)),
		PolicyRules: make([]index.PolicyRule, 0, len(doc.PolicyRules)), CIPolicies: make([]index.CIPolicy, 0, len(doc.CIPolicies))}
	for _, r := range doc.Repos {
		cp.Repos = append(cp.Repos, index.Repo{ID: r.ID, Format: r.Format, Name: r.Name, Registry: r.Registry, Prefix: r.Prefix,
			Kind: r.Kind, AnonymousRead: r.AnonymousRead, Policy: r.Policy, Upstream: r.Upstream, Group: r.Group,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Revision: r.Revision})
	}
	for _, g := range doc.GroupMembers {
		cp.GroupMembers = append(cp.GroupMembers, index.GroupMember{GroupID: g.Group, MemberID: g.Member, Position: g.Position})
	}
	for _, u := range doc.Users {
		cp.Users = append(cp.Users, index.User{ID: u.ID, UID: u.UID, Name: u.Name, PasswordHash: u.PasswordHash, Kind: u.Kind,
			Disabled: u.Disabled, CreatedAt: u.CreatedAt, DeletedAt: u.DeletedAt, DeletedName: u.DeletedName, Owner: u.Owner,
			Revision: u.Revision, LastSeenAt: u.LastSeenAt, MustChange: u.MustChange, PasswordHistory: u.PasswordHistory})
	}
	for _, i := range doc.Identities {
		cp.Identities = append(cp.Identities, index.Identity{ID: i.ID, UserID: i.User, Issuer: i.Issuer, Subject: i.Subject,
			CreatedAt: i.CreatedAt, LoginAt: i.LoginAt, GroupsExpireAt: i.GroupsExpireAt})
	}
	for _, g := range doc.IdentityGroups {
		cp.IdentityGroups = append(cp.IdentityGroups, index.IdentityGroup{IdentityID: g.Identity, Name: g.Name})
	}
	for _, t := range doc.Tokens {
		cp.Tokens = append(cp.Tokens, index.Token{ID: t.ID, Kind: t.Kind, UserID: t.User, RepoID: t.Repo, RepoMask: t.RepoMask,
			Name: t.Name, Hash: t.Hash, Scopes: t.Scopes, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, CreatedAt: t.CreatedAt,
			RevokedAt: t.RevokedAt, DPoPJKT: t.DPoPJKT, AuthTime: t.AuthTime, AMR: t.AMR, ACR: t.ACR})
	}
	for _, b := range doc.Bindings {
		cp.Bindings = append(cp.Bindings, index.Binding{ID: b.ID, Principal: b.Principal, Role: b.Role, RepoMask: b.RepoMask,
			CreatedAt: b.CreatedAt})
	}
	for _, k := range doc.Keys {
		cp.Keys = append(cp.Keys, index.Key{ID: k.ID, RepoID: k.Repo, Kind: k.Kind, Public: k.Public, PrivateEnc: k.PrivateEnc,
			Fingerprint: k.Fingerprint, CreatedAt: k.CreatedAt, RetiredAt: k.RetiredAt})
	}
	for _, h := range doc.Webhooks {
		cp.Webhooks = append(cp.Webhooks, index.Webhook{ID: h.ID, UID: h.UID, Mask: h.Mask, URL: h.URL, Events: h.Events,
			SecretEnc: h.SecretEnc, Active: h.Active, Description: h.Description, CreatedBy: h.CreatedBy, CreatedAt: h.CreatedAt,
			UpdatedAt: h.UpdatedAt, Revision: h.Revision})
	}
	for _, r := range doc.PolicyRules {
		cp.PolicyRules = append(cp.PolicyRules, index.PolicyRule{ID: r.ID, RepoMask: r.RepoMask, Name: r.Name, Version: r.Version,
			Variant: r.Variant, Effect: r.Effect, Reason: r.Reason, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt})
	}
	for _, p := range doc.CIPolicies {
		cp.CIPolicies = append(cp.CIPolicies, index.CIPolicy{ID: p.ID, Name: p.Name, Issuer: p.Issuer, Claims: p.Claims, Grants: p.Grants,
			State: p.State, ExpiresAt: p.ExpiresAt, RenewalExpiresAt: p.RenewalExpiresAt, CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt,
			UpdatedBy: p.UpdatedBy, UpdatedAt: p.UpdatedAt, ApprovedBy: p.ApprovedBy, ApprovedAt: p.ApprovedAt, Revision: p.Revision})
	}
	cp.SealingKey = doc.SealingKey.row()
	if doc.Audit != nil {
		cp.WithAudit, cp.Audit = true, []index.AuditEvent{}
		for _, e := range *doc.Audit {
			cp.Audit = append(cp.Audit, index.AuditEvent{ID: e.ID, At: e.At, Principal: e.Principal, PrincipalName: e.PrincipalName,
				Action: e.Action, Repo: e.Repo, Subject: e.Subject, Digest: e.Digest, IP: e.IP, Details: e.Details, State: e.State,
				Error: e.Error})
		}
		if doc.AuditSince != nil {
			cp.AuditSince = *doc.AuditSince
		}
	}
	return cp
}
