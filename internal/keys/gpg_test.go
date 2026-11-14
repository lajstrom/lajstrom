package keys

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// aptPubkeyAlgos is what apt 2.7 and later pass to gpgv as --assert-pubkey-algo
// (APT::Key::Assert-Pubkey-Algo): the algorithms a repo's key may have.
const aptPubkeyAlgos = ">=rsa2048,ed25519,ed448,nistp256,nistp384,nistp521"

// gpgv returns a function that runs gpgv on the files of dir with the binary keyring of
// armored, and with apt's --assert-pubkey-algo when this gpgv has it; it skips the test when
// there is no gpgv.
func gpgv(t *testing.T, dir string, armored []byte) func(files ...string) string {
	t.Helper()
	bin, err := exec.LookPath("gpgv")
	if err != nil {
		t.Skip("no gpgv in PATH")
	}
	block, err := armor.Decode(bytes.NewReader(armored))
	must(t, err)
	ring := must2(io.ReadAll(block.Body))
	must(t, os.WriteFile(filepath.Join(dir, "keyring.gpg"), ring, 0o600))
	home := t.TempDir()
	args := []string{"--homedir", home, "--keyring", filepath.Join(dir, "keyring.gpg")}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	opts, _ := exec.CommandContext(ctx, bin, "--dump-options").Output() //nolint:gosec // the gpgv of PATH
	if bytes.Contains(opts, []byte("--assert-pubkey-algo")) {
		args = append(args, "--assert-pubkey-algo="+aptPubkeyAlgos)
	}
	return func(files ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, bin, append(args, files...)...) //nolint:gosec // gpgv on the test's own files
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gpgv %s: %v\n%s", strings.Join(files, " "), err, out)
		}
		return string(out)
	}
}

// TestKMSGPGVerify: gpgv, what apt verifies with, takes the InRelease, the Release.gpg and an
// rpm repo's repomd.xml.asc that a KMS key signs, RSA and P-256, under apt's
// --assert-pubkey-algo, and so it does after a rotation from a key made here, both signing.
func TestKMSGPGVerify(t *testing.T) {
	const repomd = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<repomd xmlns="http://linux.duke.edu/metadata/repo"><revision>1</revision></repomd>` + "\n"
	for _, tt := range []struct {
		name, key string
		rotate    bool
	}{
		{"rsa2048", "rsa2048", false},
		{"p256", "p256", false},
		{"rotated onto p256", "p256", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			kms := "awskms:///" + kmsAlias
			if tt.rotate {
				kms = ""
			}
			e, _ := newKMSEnv(t, kmsKeys()[tt.key], kms)
			// gpgv refuses a key or a signature from the future of its clock, and the env's
			// clock is 60 days back: the keys and the signatures are made an hour ago.
			e.now = time.Now().Add(-time.Hour)
			if tt.rotate {
				must2(e.svc.KeyFor(ctx, e.repo))
				e.now = e.now.Add(time.Minute)
				must2(e.svc.RotateTo(ctx, e.repo, "awskms:///"+kmsARN))
			}
			dir := t.TempDir()
			files := map[string][]byte{
				"Release":        []byte(release),
				"InRelease":      must2(e.svc.ClearSign(ctx, e.repo, []byte(release))),
				"Release.gpg":    must2(e.svc.DetachSign(ctx, e.repo, []byte(release))),
				"repomd.xml":     []byte(repomd),
				"repomd.xml.asc": must2(e.svc.DetachSign(ctx, e.repo, []byte(repomd))),
			}
			for name, b := range files {
				must(t, os.WriteFile(filepath.Join(dir, name), b, 0o600))
			}
			verify := gpgv(t, dir, must2(e.svc.PublicKeys(ctx, e.repo)))
			want := 1
			if tt.rotate {
				want = 2
			}
			for _, args := range [][]string{{"InRelease"}, {"Release.gpg", "Release"}, {"repomd.xml.asc", "repomd.xml"}} {
				if out := verify(args...); strings.Count(out, "Good signature") != want {
					t.Errorf("gpgv %v: want %d good signatures:\n%s", args, want, out)
				}
			}
		})
	}
}
