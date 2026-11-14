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
	// Audit is nil when the snapshot was made without audit rows, and absent then.
	Audit *[]auditRow `json:"audit,omitempty"`
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
	Name       string     `json:"name,omitempty"`
	Hash       string     `json:"hash"`
	Scopes     []string   `json:"scopes"` // null and [] apart, as the table has them
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
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
		Bindings: make([]bindingRow, 0, len(cp.Bindings)), Keys: make([]keyRow, 0, len(cp.Keys))}
	for _, r := range cp.Repos {
		doc.Repos = append(doc.Repos, repoRow{ID: r.ID, Format: r.Format, Name: r.Name, Registry: r.Registry, Prefix: r.Prefix,
			Kind: r.Kind, AnonymousRead: r.AnonymousRead, Policy: r.Policy, Upstream: r.Upstream, Group: r.Group,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
	}
	for _, g := range cp.GroupMembers {
		doc.GroupMembers = append(doc.GroupMembers, groupMemberRow{Group: g.GroupID, Member: g.MemberID, Position: g.Position})
	}
	for _, u := range cp.Users {
		doc.Users = append(doc.Users, userRow{ID: u.ID, UID: u.UID, Name: u.Name, PasswordHash: u.PasswordHash, Kind: u.Kind,
			Disabled: u.Disabled, CreatedAt: u.CreatedAt, DeletedAt: u.DeletedAt, DeletedName: u.DeletedName})
	}
	for _, i := range cp.Identities {
		doc.Identities = append(doc.Identities, identityRow{ID: i.ID, User: i.UserID, Issuer: i.Issuer, Subject: i.Subject,
			CreatedAt: i.CreatedAt, LoginAt: i.LoginAt, GroupsExpireAt: i.GroupsExpireAt})
	}
	for _, g := range cp.IdentityGroups {
		doc.IdentityGroups = append(doc.IdentityGroups, identityGroupRow{Identity: g.IdentityID, Name: g.Name})
	}
	for _, t := range cp.Tokens {
		doc.Tokens = append(doc.Tokens, tokenRow{ID: t.ID, Kind: t.Kind, User: t.UserID, Repo: t.RepoID, Name: t.Name, Hash: t.Hash,
			Scopes: t.Scopes, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, CreatedAt: t.CreatedAt, RevokedAt: t.RevokedAt})
	}
	for _, b := range cp.Bindings {
		doc.Bindings = append(doc.Bindings, bindingRow{ID: b.ID, Principal: b.Principal, Role: b.Role, RepoMask: b.RepoMask,
			CreatedAt: b.CreatedAt})
	}
	for _, k := range cp.Keys {
		doc.Keys = append(doc.Keys, keyRow{ID: k.ID, Repo: k.RepoID, Kind: k.Kind, Public: k.Public, PrivateEnc: k.PrivateEnc,
			Fingerprint: k.Fingerprint, CreatedAt: k.CreatedAt, RetiredAt: k.RetiredAt})
	}
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
		Keys: make([]index.Key, 0, len(doc.Keys))}
	for _, r := range doc.Repos {
		cp.Repos = append(cp.Repos, index.Repo{ID: r.ID, Format: r.Format, Name: r.Name, Registry: r.Registry, Prefix: r.Prefix,
			Kind: r.Kind, AnonymousRead: r.AnonymousRead, Policy: r.Policy, Upstream: r.Upstream, Group: r.Group,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
	}
	for _, g := range doc.GroupMembers {
		cp.GroupMembers = append(cp.GroupMembers, index.GroupMember{GroupID: g.Group, MemberID: g.Member, Position: g.Position})
	}
	for _, u := range doc.Users {
		cp.Users = append(cp.Users, index.User{ID: u.ID, UID: u.UID, Name: u.Name, PasswordHash: u.PasswordHash, Kind: u.Kind,
			Disabled: u.Disabled, CreatedAt: u.CreatedAt, DeletedAt: u.DeletedAt, DeletedName: u.DeletedName})
	}
	for _, i := range doc.Identities {
		cp.Identities = append(cp.Identities, index.Identity{ID: i.ID, UserID: i.User, Issuer: i.Issuer, Subject: i.Subject,
			CreatedAt: i.CreatedAt, LoginAt: i.LoginAt, GroupsExpireAt: i.GroupsExpireAt})
	}
	for _, g := range doc.IdentityGroups {
		cp.IdentityGroups = append(cp.IdentityGroups, index.IdentityGroup{IdentityID: g.Identity, Name: g.Name})
	}
	for _, t := range doc.Tokens {
		cp.Tokens = append(cp.Tokens, index.Token{ID: t.ID, Kind: t.Kind, UserID: t.User, RepoID: t.Repo, Name: t.Name, Hash: t.Hash,
			Scopes: t.Scopes, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, CreatedAt: t.CreatedAt, RevokedAt: t.RevokedAt})
	}
	for _, b := range doc.Bindings {
		cp.Bindings = append(cp.Bindings, index.Binding{ID: b.ID, Principal: b.Principal, Role: b.Role, RepoMask: b.RepoMask,
			CreatedAt: b.CreatedAt})
	}
	for _, k := range doc.Keys {
		cp.Keys = append(cp.Keys, index.Key{ID: k.ID, RepoID: k.Repo, Kind: k.Kind, Public: k.Public, PrivateEnc: k.PrivateEnc,
			Fingerprint: k.Fingerprint, CreatedAt: k.CreatedAt, RetiredAt: k.RetiredAt})
	}
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
