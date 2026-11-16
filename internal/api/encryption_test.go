package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

func publicPEM(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// TestCheckEncryption: policy.encryption is {mode, key, recipients}, at-rest not on an oci repo,
// key a KMS key URI or "", recipients at most 8 PEM public keys of the kinds JWE wraps for.
func TestCheckEncryption(t *testing.T) {
	r, _ := rsa.GenerateKey(rand.Reader, 2048)
	weak, _ := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // a weak key, to be refused
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rsaPEM, _ := json.Marshal(publicPEM(t, &r.PublicKey))
	ecPEM, _ := json.Marshal(publicPEM(t, &ec.PublicKey))
	weakPEM, _ := json.Marshal(publicPEM(t, &weak.PublicKey))
	nine := "[" + strings.TrimSuffix(strings.Repeat(string(rsaPEM)+",", 9), ",") + "]"
	one := `,"recipients":[` + string(rsaPEM) + `]`
	for _, tt := range []struct {
		name, format, in, err string
	}{
		{"at-rest", "raw", `{"encryption":{"mode":"at-rest"` + one + `}}`, ""},
		{"none", "oci", `{"encryption":{"mode":"none"}}`, ""},
		{"a KMS key", "apt", `{"encryption":{"mode":"at-rest","key":"awskms:///alias/lajstrom-secrets"` + one + `}}`, ""},
		{"a GCP key", "pypi", `{"encryption":{"mode":"at-rest","key":"gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"` + one + `}}`, ""},
		{"recipients", "raw", `{"encryption":{"mode":"at-rest","recipients":[` + string(rsaPEM) + `,` + string(ecPEM) + `]}}`, ""},
		{"an empty key", "raw", `{"encryption":{"mode":"at-rest","key":""` + one + `}}`, ""},
		{"none without recipients", "raw", `{"encryption":{"mode":"none","key":""}}`, ""},
		{"at-rest without recipients", "raw", `{"encryption":{"mode":"at-rest"}}`, "policy.encryption.recipients: at-rest needs at least one recipient, the recovery recipient"},
		{"at-rest with no recipient", "raw", `{"encryption":{"mode":"at-rest","recipients":[]}}`, "policy.encryption.recipients: at-rest needs at least one"},
		{"no mode", "raw", `{"encryption":{"key":""}}`, `policy.encryption.mode: must be "none" or "at-rest"`},
		{"another mode", "raw", `{"encryption":{"mode":"on"}}`, `policy.encryption.mode: must be`},
		{"at-rest on oci", "oci", `{"encryption":{"mode":"at-rest"}}`, "policy.encryption.mode: an oci repo passes"},
		{"not an object", "raw", `{"encryption":"at-rest"}`, "policy.encryption: must be a JSON object"},
		{"unknown key", "raw", `{"encryption":{"mode":"at-rest","cipher":"aes"}}`, "policy.encryption.cipher: unknown key; the keys are key, mode, recipients"},
		{"a key that is no URI", "raw", `{"encryption":{"mode":"at-rest","key":"alias/x"}}`, "policy.encryption.key: "},
		{"a GCP key without version", "raw", `{"encryption":{"mode":"at-rest","key":"gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k"}}`, "policy.encryption.key: "},
		{"a key as a number", "raw", `{"encryption":{"mode":"at-rest","key":1}}`, "policy.encryption.key: must be"},
		{"recipients not a list", "raw", `{"encryption":{"mode":"at-rest","recipients":"x"}}`, "policy.encryption.recipients: must be a list"},
		{"nine recipients", "raw", `{"encryption":{"mode":"at-rest","recipients":` + nine + `}}`, "policy.encryption.recipients: must be a list of at most 8"},
		{"a weak recipient", "raw", `{"encryption":{"mode":"at-rest","recipients":[` + string(weakPEM) + `]}}`, "policy.encryption.recipients[0]: an RSA key of 1024 bits"},
		{"a recipient not PEM", "raw", `{"encryption":{"mode":"at-rest","recipients":["hello"]}}`, "policy.encryption.recipients[0]: not a PEM block"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkPolicy(tt.format, json.RawMessage(tt.in))
			if tt.err == "" {
				if err != nil {
					t.Fatalf("error %v", err)
				}
				if p, err := format.EncryptionOf(repoWith(tt.format, got)); err != nil || p.Mode == "" {
					t.Errorf("EncryptionOf(%s) = %+v, %v", got, p, err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tt.err) || !errors.Is(err, format.ErrInvalid) {
				t.Errorf("error %v, want an INVALID error starting %q", err, tt.err)
			}
		})
	}
}

// TestAtRestNeedsTheMasterKey: without it a repo cannot be created with encryption at rest, nor
// changed to it; with it both are.
func TestAtRestNeedsTheMasterKey(t *testing.T) {
	ctx := context.Background()
	atRest := atRestFor(t)
	for _, master := range []bool{false, true} {
		s := &Repos{Index: newStore(t), Formats: oneOf("raw"), Registries: oneOf("local"), Encryption: master}
		_, err := s.Create(ctx, CreateRequest{Format: "raw", Name: "secrets", Registry: "local", Policy: atRest}, Origin{})
		if master != (err == nil) {
			t.Errorf("master key %v: Create with at-rest: %v", master, err)
		}
		if _, err := s.Create(ctx, CreateRequest{Format: "raw", Name: "plain", Registry: "local"}, Origin{}); err != nil {
			t.Fatal(err)
		}
		_, err = s.Update(ctx, "raw", "plain", UpdateRequest{Policy: atRest}, Origin{})
		if master != (err == nil) {
			t.Errorf("master key %v: Update to at-rest: %v", master, err)
		}
		if !master && (err == nil || !strings.Contains(err.Error(), "needs the master key")) {
			t.Errorf("the refusal does not say why: %v", err)
		}
	}
}

// TestAtRestNeedsARecipient: a repo is neither created with encryption at rest and no recipient,
// nor changed to it, nor left without its last recipient, by the API or by the configuration's
// bootstrap; a change to at-rest keeps the recipients the policy has (decided on #114).
func TestAtRestNeedsARecipient(t *testing.T) {
	ctx := context.Background()
	s := &Repos{Index: newStore(t), Formats: oneOf("raw"), Registries: oneOf("local"), Encryption: true}
	bare := json.RawMessage(`{"encryption":{"mode":"at-rest"}}`)
	const why = "policy.encryption.recipients: at-rest needs at least one recipient"
	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), why) || !errors.Is(err, format.ErrInvalid) {
			t.Errorf("%s: %v, want an INVALID error saying %q", what, err, why)
		}
	}
	_, err := s.Create(ctx, CreateRequest{Format: "raw", Name: "secrets", Registry: "local", Policy: bare}, Origin{})
	refused("Create with at-rest and no recipient", err)
	if _, err := s.Create(ctx, CreateRequest{Format: "raw", Name: "plain", Registry: "local"}, Origin{}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Update(ctx, "raw", "plain", UpdateRequest{Policy: bare}, Origin{})
	refused("Update to at-rest with no recipient", err)

	var p struct {
		Encryption format.EncryptionPolicy `json:"encryption"`
	}
	if err := json.Unmarshal(atRestFor(t), &p); err != nil {
		t.Fatal(err)
	}
	p.Encryption.Mode = format.EncryptionNone
	withRecipient, _ := json.Marshal(p)
	if _, err := s.Create(ctx, CreateRequest{Format: "raw", Name: "later", Registry: "local", Policy: withRecipient}, Origin{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, "raw", "later", UpdateRequest{Policy: bare}, Origin{}); err != nil {
		t.Errorf("Update to at-rest of a repo with a recipient: %v", err)
	}
	_, err = s.Update(ctx, "raw", "later", UpdateRequest{Policy: json.RawMessage(`{"encryption":{"recipients":[]}}`)}, Origin{})
	refused("Update that removes the last recipient of an at-rest repo", err)

	_, err = s.Bootstrap(ctx, []config.Repo{{Format: "raw", Name: "declared", Registry: "local",
		Policy: map[string]any{"encryption": map[string]any{"mode": "at-rest"}}}})
	refused("Bootstrap of a configured repo with at-rest and no recipient", err)
}

// atRestFor is a policy with encryption at rest for one recipient, a new RSA key.
func atRestFor(t *testing.T) json.RawMessage {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemText, _ := json.Marshal(publicPEM(t, &k.PublicKey))
	return json.RawMessage(`{"encryption":{"mode":"at-rest","recipients":[` + string(pemText) + `]}}`)
}

// repoWith is a hosted repo of formatName with the policy p.
func repoWith(formatName string, p json.RawMessage) index.Repo {
	return index.Repo{Format: formatName, Name: "r", Kind: index.KindHosted, Policy: p}
}
