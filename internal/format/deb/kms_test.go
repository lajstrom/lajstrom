package deb

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

// TestRoutesKMS signs with a key held in a KMS (ADR 0020): InRelease and Release.gpg verify
// against key.asc; while the KMS is unavailable after a publish they are 503 with Retry-After:
// 5, the cause in the log only, the Release is still served and nothing signed is stored; once
// it is back they are signed; a KMS that refuses is 500.
func TestRoutesKMS(t *testing.T) {
	kms := keystest.NewKMS(time.Now().Add(-24 * time.Hour))
	e := newAptEnv(t, envOptions{signer: func(st index.Store) format.Signer {
		m, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
		must(t, err)
		svc, err := keys.New(keys.Options{Index: st, Master: m, RetireGrace: 720 * time.Hour, Log: slog.New(slog.DiscardHandler),
			KMS: map[string]keys.KMS{config.KMSSchemeAWS: kms}, KMSKey: kms.URI()})
		must(t, err)
		return svc
	}})
	fixtureSet(t, e.pipeline)
	kr := e.keyring()
	e.checkSigned("jammy", kr)
	signs := kms.Signs()

	newer, _ := testutil.Deb(testutil.DebOptions{Version: "1.2.0-1"})
	e.uploadTo(t, newer, "jammy", "main")
	kms.Fail(fmt.Errorf("%w: dial tcp: connection refused", keys.ErrKMSUnavailable))
	for _, file := range []string{"InRelease", "Release.gpg"} {
		rr := e.do(http.MethodGet, "dists/jammy/"+file)
		if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != "5" || strings.Contains(rr.Body.String(), "refused") {
			t.Errorf("%s with the KMS unavailable = %d, Retry-After %q, %q; want 503, 5, no cause", file, rr.Code,
				rr.Header().Get("Retry-After"), rr.Body)
		}
		if _, err := e.st.GetIndexCache(t.Context(), e.repo.ID, "jammy", file); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("%s stored while the KMS was unavailable: %v", file, err)
		}
	}
	if rr := e.do(http.MethodGet, "dists/jammy/Release"); rr.Code != http.StatusOK {
		t.Errorf("Release with the KMS unavailable = %d", rr.Code)
	}
	if kms.Signs() == signs {
		t.Error("the KMS was not asked")
	}

	kms.Fail(nil)
	e.checkSigned("jammy", kr)

	other, _ := testutil.Deb(testutil.DebOptions{Version: "1.3.0-1"})
	e.uploadTo(t, other, "jammy", "main")
	kms.Fail(errors.New("AccessDeniedException: not allowed to sign"))
	if rr := e.do(http.MethodGet, "dists/jammy/InRelease"); rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "Denied") {
		t.Errorf("InRelease with the KMS refusing = %d %q, want 500 without the cause", rr.Code, rr.Body)
	}
}
