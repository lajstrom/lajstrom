package keys

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

const (
	// KindGPG is the kind of a repo's signing keys in the keys table.
	KindGPG = "gpg"
	// ActionRotate is the audit action of a rotation: Subject is the new fingerprint, Details
	// {"retired": "<the old one>"}.
	ActionRotate = "key.rotate"

	// AlgorithmEd25519 makes OpenPGP v4 EdDSA keys on Curve25519, which apt verifies since 2.0
	// (gpgv 2.1 and later).
	AlgorithmEd25519 = "ed25519"
	// AlgorithmRSA4096 makes RSA keys of 4096 bits, for older apt.
	AlgorithmRSA4096 = "rsa4096"
)

// Options configure a Service.
type Options struct {
	// Index holds the keys; Rotate also invalidates the repo's index_cache and writes the
	// audit row through it.
	Index index.Store
	// Master encrypts the private keys at rest; required.
	Master *Master
	// Algorithm is AlgorithmEd25519 (the default when empty) or AlgorithmRSA4096: keys.algorithm.
	Algorithm string
	// RetireGrace is keys.retireGrace: how long a retired key keeps signing beside the active one
	// and stays in key.asc. Zero: not at all, for a key that must stop being trusted at once.
	RetireGrace time.Duration
	// Now is the clock of key creation, signatures and the grace; nil is time.Now.
	Now func() time.Time
	// Log gets one line per key made and per rotation, with fingerprints only; nil is
	// slog.Default().
	Log *slog.Logger
}

// Service keeps the signing keys of the repos: one active OpenPGP key per repo, made on first
// use, its private key encrypted with the master key in keys.private_enc (nonce ||
// AES-256-GCM, the fingerprint as additional data), its public key packets in keys.public. It
// implements format.Signer. A rotation retires the active key; for RetireGrace after that the
// retired key still signs beside the new one and stays in key.asc, so a client that pinned the
// keyring before the rotation verifies the documents signed after it until it fetches the
// keyring again, and a document signed before the rotation verifies against the new keyring.
// The service holds no state between calls: replicas share the keys through the index.
type Service struct {
	st        index.Store
	master    *Master
	algorithm string
	grace     time.Duration
	now       func() time.Time
	log       *slog.Logger
}

var _ format.Signer = (*Service)(nil)

// New returns the Service of o. A missing Index or Master, an unknown Algorithm or a negative
// RetireGrace is an error.
func New(o Options) (*Service, error) {
	s := &Service{st: o.Index, master: o.Master, algorithm: cmp.Or(o.Algorithm, AlgorithmEd25519),
		grace: o.RetireGrace, now: o.Now, log: o.Log}
	switch {
	case s.st == nil:
		return nil, errors.New("keys: no index")
	case s.master == nil:
		return nil, errors.New("keys: no master key")
	case s.algorithm != AlgorithmEd25519 && s.algorithm != AlgorithmRSA4096:
		return nil, fmt.Errorf("keys: algorithm %q is not %s or %s", s.algorithm, AlgorithmEd25519, AlgorithmRSA4096)
	case s.grace < 0:
		return nil, errors.New("keys: the retire grace is negative")
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Entity is a signing key: its row in keys and the OpenPGP entity with the private key
// decrypted. It prints as its fingerprint, never with the key material.
type Entity struct {
	// Key is the row; PrivateEnc is the ciphertext.
	Key index.Key
	// OpenPGP is the key, private key decrypted.
	OpenPGP *openpgp.Entity
}

// String returns "gpg key <fingerprint>".
func (e *Entity) String() string { return "gpg key " + e.Key.Fingerprint }

// GoString returns the same for %#v.
func (e *Entity) GoString() string { return e.String() }

// LogValue logs the fingerprint only.
func (e *Entity) LogValue() slog.Value { return slog.StringValue(e.Key.Fingerprint) }

// KeyFor returns the active gpg key of repo, decrypted. A repo without one gets it now: an
// OpenPGP key of Options.Algorithm with the user id "Lajstrom <format>/<name>", no expiry and
// SHA-256 as its hash. Two replicas that make the first key of a repo at once both insert it;
// the unique index keys_active keeps one, and the replica whose insert fails reads that one.
func (s *Service) KeyFor(ctx context.Context, repo index.Repo) (*Entity, error) {
	k, err := s.active(ctx, repo)
	if err != nil {
		return nil, err
	}
	return s.decrypt(*k)
}

// ClearSign returns data clearsigned by the repo's signing keys (Signing), as gpg --clearsign
// writes it and apt reads an InRelease: the "Hash: SHA256" header, data as the cleartext, its
// last line break the one before the signature, and one signature per key.
func (s *Service) ClearSign(ctx context.Context, repo index.Repo, data []byte) ([]byte, error) {
	signers, err := s.Signing(ctx, repo)
	if err != nil {
		return nil, err
	}
	privs := make([]*packet.PrivateKey, len(signers))
	for i, e := range signers {
		privs[i] = e.OpenPGP.PrivateKey
	}
	var out bytes.Buffer
	w, err := clearsign.EncodeMulti(&out, privs, s.signConfig())
	if err == nil {
		_, _ = w.Write(bytes.TrimSuffix(data, []byte("\n"))) // into a bytes.Buffer: Close reports a failure
		err = w.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("keys: clearsign for %s: %w", repoName(repo), err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// DetachSign returns the armored detached signatures of data by the repo's signing keys
// (Signing), one armor block with one binary signature per key, as apt reads a Release.gpg.
func (s *Service) DetachSign(ctx context.Context, repo index.Repo, data []byte) ([]byte, error) {
	signers, err := s.Signing(ctx, repo)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	w, _ := armor.Encode(&out, openpgp.SignatureType, nil) // into a bytes.Buffer: never an error
	for _, e := range signers {
		if err := openpgp.DetachSign(w, e.OpenPGP, bytes.NewReader(data), s.signConfig()); err != nil {
			return nil, fmt.Errorf("keys: sign for %s with %s: %w", repoName(repo), e.Key.Fingerprint, err)
		}
	}
	_ = w.Close() // the armor footer into a bytes.Buffer: never an error
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// PublicKeys returns the armored keyring of the repo, what apt's signed-by= points at: one
// public key block with the active key, made now when the repo has none, and the keys retired
// within the grace, the most recently retired first.
func (s *Service) PublicKeys(ctx context.Context, repo index.Repo) ([]byte, error) {
	keys, err := s.keyring(ctx, repo)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	w, _ := armor.Encode(&out, openpgp.PublicKeyType, nil) // into a bytes.Buffer: never an error
	for _, k := range keys {
		_, _ = w.Write(k.Public)
	}
	_ = w.Close()
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// Signing returns the keys that sign the repo's documents, decrypted, in the order of
// PublicKeys: the active key, made now when the repo has none, and the keys retired within
// the grace. An active key that does not decrypt is an error; a retired one is left out with a
// warning, so that a repo whose keys were made under a master key that is lost signs with the
// key a rotation under the new master key made.
func (s *Service) Signing(ctx context.Context, repo index.Repo) ([]*Entity, error) {
	keys, err := s.keyring(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := make([]*Entity, 0, len(keys))
	for i, k := range keys {
		e, err := s.decrypt(k)
		switch {
		case err == nil:
			out = append(out, e)
		case i == 0:
			return nil, err
		default:
			s.log.WarnContext(ctx, "retired signing key left out: it does not decrypt", "repo", repoName(repo),
				"fingerprint", k.Fingerprint, "error", err.Error())
		}
	}
	return out, nil
}

// Current reports whether signed, an InRelease or a Release.gpg that ClearSign or DetachSign
// made for the repo, is signed by the active key, and otherwise only by keys retired within
// the grace. A rotation drops the documents it changes, but the end of a retired key's grace
// is a time, not a write, so nothing drops the documents that key signed: the routes that
// serve them from index_cache ask Current on every read and sign again when it is false, on
// whichever replica serves next, with or without the reconcile loop (#577). The signatures
// name the keys that made them, so a grace shortened since counts too. A document without
// signatures, or with one that does not name its issuer's fingerprint, is not current. It
// reads the keys once and decrypts none.
func (s *Service) Current(ctx context.Context, repo index.Repo, signed []byte) (bool, error) {
	all, err := s.st.ListKeys(ctx, &repo.ID)
	if err != nil {
		return false, fmt.Errorf("keys: the keys of %s: %w", repoName(repo), err)
	}
	since := s.graceStart()
	signing := make(map[string]bool, len(all)) // by fingerprint: true for the active key
	for _, k := range all {
		if k.Kind == KindGPG && (k.RetiredAt == nil || !k.RetiredAt.Before(since)) {
			signing[k.Fingerprint] = k.RetiredAt == nil
		}
	}
	active := false
	for _, fp := range signers(signed) {
		isActive, signs := signing[fp]
		if !signs {
			return false, nil
		}
		active = active || isActive
	}
	return active, nil
}

// signers returns the fingerprints of the issuers of the signatures in signed: the signature
// block of a clearsigned document, or an armored signature block. It is nil when signed is
// neither, or holds a packet that is not a signature or a signature without its issuer's
// fingerprint, which ClearSign and DetachSign always write.
func signers(signed []byte) []string {
	var body io.Reader
	if b, _ := clearsign.Decode(signed); b != nil {
		body = b.ArmoredSignature.Body
	} else if block, err := armor.Decode(bytes.NewReader(signed)); err == nil && block.Type == openpgp.SignatureType {
		body = block.Body
	} else {
		return nil
	}
	var fps []string
	r := packet.NewReader(body)
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return fps
		}
		sig, ok := p.(*packet.Signature)
		if err != nil || !ok || len(sig.IssuerFingerprint) == 0 {
			return nil
		}
		fps = append(fps, strings.ToUpper(hex.EncodeToString(sig.IssuerFingerprint)))
	}
}

// Rotate gives the repo a new active key and retires the one it had, in one transaction that
// also drops the repo's index_cache, so that the next GET renders and signs again, and writes
// the audit row key.rotate (Repo the repo, Subject the new fingerprint, Details the retired
// one). The new key is made before the transaction; a repo without a key gets its first one.
// A rotation that loses to another at the same time is an error wrapping index.ErrConflict.
func (s *Service) Rotate(ctx context.Context, repo index.Repo) (*Entity, error) {
	row, key, err := s.generate(repo)
	if err != nil {
		return nil, err
	}
	var retired string
	err = s.st.Tx(ctx, func(tx index.Writer) error {
		retired = ""
		old, err := tx.GetKey(ctx, &repo.ID, KindGPG)
		switch {
		case err == nil:
			if err := tx.RetireKey(ctx, old.ID, s.now()); err != nil {
				return err
			}
			retired = old.Fingerprint
		case !errors.Is(err, index.ErrNotFound):
			return err
		}
		if err := tx.CreateKey(ctx, row); err != nil {
			return err
		}
		if err := tx.InvalidateIndexCache(ctx, repo.ID, ""); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, s.rotateEvent(ctx, repo, row.Fingerprint, retired))
	})
	if err != nil {
		return nil, fmt.Errorf("keys: rotate the key of %s: %w", repoName(repo), err)
	}
	s.log.InfoContext(ctx, "signing key rotated", "repo", repoName(repo), "fingerprint", row.Fingerprint,
		"retired", retired, "algorithm", s.algorithm)
	return &Entity{Key: *row, OpenPGP: key}, nil
}

// rotateEvent is the audit row of a rotation by the principal of ctx.
func (s *Service) rotateEvent(ctx context.Context, repo index.Repo, fingerprint, retired string) *index.AuditEvent {
	var details json.RawMessage
	if retired != "" {
		details, _ = json.Marshal(map[string]string{"retired": retired}) // a map of strings: never an error
	}
	return &index.AuditEvent{At: s.now(), Principal: auth.PrincipalFrom(ctx).String(), Action: ActionRotate,
		Repo: repoName(repo), Subject: fingerprint, Details: details}
}

// active returns the row of the repo's active gpg key, made and stored when there is none.
func (s *Service) active(ctx context.Context, repo index.Repo) (*index.Key, error) {
	k, err := s.st.GetKey(ctx, &repo.ID, KindGPG)
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, index.ErrNotFound) {
		return nil, fmt.Errorf("keys: the key of %s: %w", repoName(repo), err)
	}
	row, _, err := s.generate(repo)
	if err != nil {
		return nil, err
	}
	err = s.st.CreateKey(ctx, row)
	switch {
	case err == nil:
		s.log.InfoContext(ctx, "signing key made", "repo", repoName(repo), "fingerprint", row.Fingerprint,
			"algorithm", s.algorithm)
		return row, nil
	case !errors.Is(err, index.ErrConflict):
		return nil, fmt.Errorf("keys: store the key of %s: %w", repoName(repo), err)
	}
	// Another replica stored the repo's first key between the read and the insert.
	if k, err = s.st.GetKey(ctx, &repo.ID, KindGPG); err != nil {
		return nil, fmt.Errorf("keys: the key of %s after a concurrent insert: %w", repoName(repo), err)
	}
	return k, nil
}

// keyring returns the rows of the repo's keys that sign and verify: the active one, made when
// there is none, then those retired within the grace, the most recently retired first.
func (s *Service) keyring(ctx context.Context, repo index.Repo) ([]index.Key, error) {
	if _, err := s.active(ctx, repo); err != nil {
		return nil, err
	}
	all, err := s.st.ListKeys(ctx, &repo.ID)
	if err != nil {
		return nil, fmt.Errorf("keys: the keys of %s: %w", repoName(repo), err)
	}
	since := s.graceStart()
	var active, retired []index.Key
	for _, k := range all {
		switch {
		case k.Kind != KindGPG:
		case k.RetiredAt == nil:
			active = append(active, k)
		case !k.RetiredAt.Before(since):
			retired = append(retired, k)
		}
	}
	if len(active) == 0 { // retired by a rotation between the two reads, which made another
		return nil, fmt.Errorf("keys: %s has no active key; it was rotated meanwhile, try again", repoName(repo))
	}
	slices.SortFunc(retired, func(a, b index.Key) int {
		return cmp.Or(b.RetiredAt.Compare(*a.RetiredAt), cmp.Compare(b.ID, a.ID))
	})
	return append(active, retired...), nil
}

// graceStart is the earliest RetiredAt of a retired key that still signs: one grace ago.
func (s *Service) graceStart() time.Time {
	// The store keeps timestamps to the microsecond; a clock with nanoseconds would put a
	// key retired exactly one grace ago a few nanoseconds outside it.
	return s.now().Add(-s.grace).Truncate(time.Microsecond)
}

// generate makes a new key for repo, as KeyFor describes, and returns its row, not yet stored,
// with the private key sealed by the master key, and the key itself.
func (s *Service) generate(repo index.Repo) (*index.Key, *openpgp.Entity, error) {
	created := s.now().UTC().Truncate(time.Second) // an OpenPGP key has a creation time in seconds
	cfg := &packet.Config{DefaultHash: crypto.SHA256, Time: func() time.Time { return created }}
	switch s.algorithm {
	case AlgorithmRSA4096:
		cfg.Algorithm, cfg.RSABits = packet.PubKeyAlgoRSA, 4096
	default:
		cfg.Algorithm, cfg.Curve = packet.PubKeyAlgoEdDSA, packet.Curve25519
	}
	e, err := openpgp.NewEntity("Lajstrom "+repoName(repo), "", "", cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("keys: make a key for %s: %w", repoName(repo), err)
	}
	var pub, priv bytes.Buffer
	if err := errors.Join(e.Serialize(&pub), e.SerializePrivateWithoutSigning(&priv, nil)); err != nil {
		return nil, nil, fmt.Errorf("keys: serialize the key for %s: %w", repoName(repo), err)
	}
	defer clear(priv.Bytes())
	fp := fingerprint(e)
	id := repo.ID
	return &index.Key{RepoID: &id, Kind: KindGPG, Public: pub.Bytes(), PrivateEnc: s.master.seal(fp, priv.Bytes()),
		Fingerprint: fp, CreatedAt: created}, e, nil
}

// decrypt returns the key of row k with its private key opened by the master key, checked to
// be the key of k's fingerprint.
func (s *Service) decrypt(k index.Key) (*Entity, error) {
	plain, err := s.master.open(k.Fingerprint, k.PrivateEnc)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	e, err := openpgp.ReadEntity(packet.NewReader(bytes.NewReader(plain)))
	switch {
	case err != nil:
		return nil, fmt.Errorf("keys: the private key of %s: %w", k.Fingerprint, err)
	case e.PrivateKey == nil || e.PrivateKey.Encrypted:
		return nil, fmt.Errorf("keys: the row of %s holds no usable private key", k.Fingerprint)
	case fingerprint(e) != k.Fingerprint:
		return nil, fmt.Errorf("keys: the row of %s holds the key %s", k.Fingerprint, fingerprint(e))
	}
	return &Entity{Key: k, OpenPGP: e}, nil
}

// signConfig is the configuration of the signatures: SHA-256, at the service's clock.
func (s *Service) signConfig() *packet.Config {
	return &packet.Config{DefaultHash: crypto.SHA256, Time: s.now}
}

// fingerprint is the fingerprint of e's primary key in uppercase hexadecimal, as gpg prints it.
func fingerprint(e *openpgp.Entity) string {
	return strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint))
}

// repoName is <format>/<name>, the repo as the audit log and the user ids name it.
func repoName(repo index.Repo) string { return repo.Format + "/" + repo.Name }
