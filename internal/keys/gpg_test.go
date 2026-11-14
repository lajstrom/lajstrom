package keys

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/cryptotest"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
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

// TestClearSignChecksum: an InRelease's signature block ends with the CRC24 line of its
// packets, as gpg --clearsign writes it, and gpgv takes it. gpgv before 2.5 (2.4.4 on Ubuntu
// 24.04) ends the base64 of an armor block only at a '=', the padding or the checksum, and
// go-crypto's clearsign writes no checksum: when the signature packets were a multiple of three
// bytes long, gpgv read on into the END line, took it for a broken packet and exited 2 under
// the good signatures (#1124). That is a KMS RSA-2048 signature one byte short, one in 128 to
// 256; nearly every KMS RSA-3072 one; during a rotation, nearly every pair of an Ed25519 and a
// KMS RSA-2048 signature, and one pair of Ed25519 signatures in 64. The randomness is fixed, so
// every run makes the same keys and documents.
func TestClearSignChecksum(t *testing.T) {
	cryptotest.SetGlobalRandom(t, 1124)
	rsa2048 := must2(rsa.GenerateKey(rand.Reader, 2048))
	rsa3072 := must2(rsa.GenerateKey(rand.Reader, 3072))
	// gpgv refuses a key or a signature from the future of its clock: a fixed time in the past.
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tt := range []struct {
		name string
		key  crypto.Signer // the KMS key
		// onto is "" when the KMS key is the repo's first key; otherwise an Ed25519 key made
		// here is, and the repo rotates onto this source, the old key signing beside the new.
		onto string
	}{
		{"kms rsa2048 one byte short", rsa2048, ""},
		{"kms rsa3072", rsa3072, ""},
		{"ed25519 rotated onto kms rsa2048", rsa2048, "awskms:///" + kmsARN},
		{"ed25519 rotated onto ed25519", rsa2048, Local},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			kms := "awskms:///" + kmsAlias
			if tt.onto != "" {
				kms = ""
			}
			e, f := newKMSEnv(t, tt.key, kms)
			f.add(kmsARN, tt.key, created, kmsAlias)
			e.now = created.Add(time.Hour)
			if tt.onto != "" {
				must2(e.svc.KeyFor(ctx, e.repo))
				e.now = e.now.Add(time.Minute)
				must2(e.svc.RotateTo(ctx, e.repo, tt.onto))
			}
			// The first InRelease whose signature packets need no base64 padding.
			var in []byte
			for i := 0; ; i++ {
				in = must2(e.svc.ClearSign(ctx, e.repo, []byte(release)))
				if body, _ := signatureBlock(t, in); len(body)%3 == 0 {
					break
				}
				if i == 4096 {
					t.Fatal("no signature a multiple of three bytes long in 4096")
				}
			}
			body, lines := signatureBlock(t, in)
			if sum := checksum(body); len(lines) < 2 || lines[len(lines)-2] != sum {
				t.Fatalf("the signature block of %d bytes does not end with its checksum %s:\n%s", len(body), sum, in)
			}
			dir := t.TempDir()
			must(t, os.WriteFile(filepath.Join(dir, "InRelease"), in, 0o600))
			verify := gpgv(t, dir, must2(e.svc.PublicKeys(ctx, e.repo)))
			want := 1
			if tt.onto != "" {
				want = 2
			}
			if out := verify("InRelease"); strings.Count(out, "Good signature") != want {
				t.Errorf("gpgv InRelease: want %d good signatures:\n%s", want, out)
			}
		})
	}
}

// signatureBlock returns the packets of the signature block of the clearsigned doc and the
// lines of its armor, from the line after its armor headers to its END line.
func signatureBlock(t *testing.T, doc []byte) ([]byte, []string) {
	t.Helper()
	b, _ := clearsign.Decode(doc)
	if b == nil {
		t.Fatalf("not clearsigned:\n%s", doc)
	}
	body := must2(io.ReadAll(b.ArmoredSignature.Body))
	_, block, ok := strings.Cut(string(doc), "-----BEGIN PGP SIGNATURE-----\n")
	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	headers := slices.Index(lines, "") // the blank line after the armor headers
	if !ok || headers < 0 {
		t.Fatalf("no signature block:\n%s", doc)
	}
	return body, lines[headers+1:]
}

// checksum returns the armor checksum line of data: '=' and the base64 of its CRC24 (RFC 9580
// §6.1).
func checksum(data []byte) string {
	const poly = 0x1864cfb
	crc := uint32(0xb704ce)
	for _, b := range data {
		crc ^= uint32(b) << 16
		for range 8 {
			crc <<= 1
			if crc&0x1000000 != 0 {
				crc ^= poly
			}
		}
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], crc)
	return "=" + base64.StdEncoding.EncodeToString(b[1:])
}
