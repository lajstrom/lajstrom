package rpm

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/keys/keystest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestSignedKMS signs repomd.xml with a key held in a KMS (ADR 0020): the signature verifies
// against key.asc; while the KMS is unavailable after a publish repomd.xml.asc is 503 with
// Retry-After: 5, repomd.xml is still served and no signature is stored; once the KMS is back
// it is signed.
func TestSignedKMS(t *testing.T) {
	kms := keystest.NewKMS(time.Now().Add(-24 * time.Hour))
	e := newEnv(t, envOptions{signer: func(st index.Store) format.Signer {
		m, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
		must(t, err)
		svc, err := keys.New(keys.Options{Index: st, Master: m, RetireGrace: 720 * time.Hour, Log: slog.New(slog.DiscardHandler),
			KMS: map[string]keys.KMS{config.KMSSchemeAWS: kms}, KMSKey: kms.URI()})
		must(t, err)
		return svc
	}})
	hello, _ := testutil.RPM(testutil.RPMOptions{})
	e.upload(hello)
	keyring := e.get("key.asc").Body.Bytes()
	e.verify(keyring)

	other, _ := testutil.RPM(testutil.RPMOptions{Name: "other"})
	e.upload(other)
	kms.Fail(fmt.Errorf("%w: ThrottlingException", keys.ErrKMSUnavailable))
	rr := e.send(http.MethodGet, "repodata/repomd.xml.asc", nil)
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != "5" {
		t.Errorf("repomd.xml.asc with the KMS unavailable = %d, Retry-After %q; want 503, 5", rr.Code, rr.Header().Get("Retry-After"))
	}
	if _, err := e.st.GetIndexCache(t.Context(), e.repo.ID, scopeRepodata, kindRepomdASC); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("repomd.xml.asc stored while the KMS was unavailable: %v", err)
	}
	e.get("repodata/repomd.xml")

	kms.Fail(nil)
	e.verify(keyring)
}
