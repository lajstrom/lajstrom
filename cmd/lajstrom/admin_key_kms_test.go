package main

import (
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/lajstrom/lajstrom/internal/keys/keystest"
)

// awsTestEnv points the AWS SDK of the process at nothing of the machine's: no shared files, no
// instance metadata, static credentials of the test, and a region; the KMS is the endpoint of
// the env map that run passes.
func awsTestEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"AWS_CONFIG_FILE": os.DevNull, "AWS_SHARED_CREDENTIALS_FILE": os.DevNull, "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_PROFILE": "", "AWS_ACCESS_KEY_ID": "AKIDTEST", "AWS_SECRET_ACCESS_KEY": "test-secret", "AWS_SESSION_TOKEN": "",
		"AWS_REGION": "eu-west-1", "AWS_ROLE_ARN": "", "AWS_WEB_IDENTITY_TOKEN_FILE": "",
	} {
		t.Setenv(k, v)
	}
}

// TestAdminKeyKMS runs admin key with keys.kms set and AWS KMS at AWS_ENDPOINT_URL_KMS: export
// makes the repo's first key from the KMS key (ECDSA, created when the KMS made it), rotate
// --local moves the repo to a key Lajstrom makes, rotate --kms back to the KMS key, whose
// fingerprint it had, and a KMS that refuses fails the rotation with its error.
func TestAdminKeyKMS(t *testing.T) {
	awsTestEnv(t)
	created := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	kms := keystest.NewKMS(created)
	srv := httptest.NewServer(keystest.AWSKMS(kms))
	t.Cleanup(srv.Close)
	cfg, _ := writeKeyConfig(t, t.TempDir())
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // the configuration the test wrote
	must(t, err)
	_, err = f.WriteString("  kms: \"" + kms.URI() + "\"\n")
	must(t, errors.Join(err, f.Close()))
	env := map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey, "AWS_ENDPOINT_URL_KMS": srv.URL}
	outs := runEnv(t, cfg, env, []step{
		{args: []string{"migrate"}},
		{args: []string{"admin", "repo", "create", "apt/ubuntu", "--registry", "local"}},
		{args: []string{"admin", "key", "export", "--repo", "apt/ubuntu"}},
		{args: []string{"admin", "key", "rotate", "--repo", "apt/ubuntu", "--local"}},
		{args: []string{"admin", "key", "rotate", "--repo", "apt/ubuntu", "--kms", "awskms:///" + kms.ARN}},
		{args: []string{"admin", "key", "rotate", "--repo", "apt/ubuntu", "--kms", "awskms:///" + kms.Alias},
			wantErr: "the active key is already"},
		{args: []string{"admin", "key", "rotate", "--token", "--kms", kms.URI()}, wantErr: "--kms and --local go with --repo"},
		{args: []string{"admin", "key", "rotate", "--repo", "apt/ubuntu", "--kms", "alias/x"}, wantErr: "--kms: \"alias/x\" is not a key URI"},
		{args: []string{"admin", "key", "rotate", "--repo", "apt/ubuntu", "--kms", kms.URI(), "--local"}, wantErr: "none of the others can be"},
	})
	el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(outs[2]))
	must(t, err)
	if len(el) != 1 || el[0].PrimaryKey.PubKeyAlgo != packet.PubKeyAlgoECDSA || !el[0].PrimaryKey.CreationTime.Equal(created) {
		t.Fatalf("the first key under keys.kms: %d keys, the first %v created %v; want ECDSA created %v", len(el),
			el[0].PrimaryKey.PubKeyAlgo, el[0].PrimaryKey.CreationTime, created)
	}
	first := fingerprintsOf(t, outs[2])[0]
	local, back := strings.TrimSpace(outs[3]), strings.TrimSpace(outs[4])
	if local == first || back != first {
		t.Errorf("first %s, --local %s, --kms %s: want a new key, then the first again", first, local, back)
	}

	kms.Fail(errors.New("AccessDeniedException"))
	runEnv(t, cfg, env, []step{{args: []string{"admin", "key", "rotate", "--repo", "apt/ubuntu", "--local"}}})
	runEnv(t, cfg, env, []step{{args: []string{"admin", "key", "rotate", "--repo", "apt/ubuntu"}, wantErr: "AccessDeniedException"}})
}
