package ecr

import (
	"bytes"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"

	"github.com/lajstrom/lajstrom/internal/provider"
)

func newTokens(f *fakeECR, log *slog.Logger) *tokens {
	return &tokens{api: f, now: f.clock.now, log: log}
}

// wantToken gets a token from s and checks its password and how many tokens were fetched.
func wantToken(t *testing.T, s *tokens, f *fakeECR, password string, fetched int) {
	t.Helper()
	tok, err := s.get(t.Context())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tok.cred.Username != "AWS" || tok.cred.Password != password {
		t.Errorf("token = %s:%s, want AWS:%s", tok.cred.Username, tok.cred.Password, password)
	}
	if got := f.count("GetAuthorizationToken"); got != fetched {
		t.Errorf("GetAuthorizationToken called %d times, want %d", got, fetched)
	}
}

// TestTokenRefresh: a 12-hour token is used for 11 hours and then replaced; a refresh that
// fails keeps the token in use until it expires, asking again at most once a minute; without a
// token that works, the failure is the error.
func TestTokenRefresh(t *testing.T) {
	c := newClock()
	f := newFakeECR(c)
	var logs bytes.Buffer
	s := newTokens(f, slog.New(slog.NewTextHandler(&logs, nil)))

	wantToken(t, s, f, "token-1", 1)
	c.advance(11*time.Hour - time.Second)
	wantToken(t, s, f, "token-1", 1)
	c.advance(time.Second)
	wantToken(t, s, f, "token-2", 2)

	c.advance(11 * time.Hour) // token-2 is due, and ECR refuses
	f.tokenErr = &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not allowed"}
	wantToken(t, s, f, "token-2", 3)
	c.advance(retryRefresh - time.Second)
	wantToken(t, s, f, "token-2", 3)
	c.advance(time.Second)
	wantToken(t, s, f, "token-2", 4)
	if !strings.Contains(logs.String(), "refresh failed") {
		t.Errorf("no warning about the failed refresh:\n%s", logs.String())
	}

	c.advance(time.Hour) // token-2 has expired
	_, err := s.get(t.Context())
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Errorf("get without a token that works = %v, want ErrUnauthorized", err)
	}
	f.tokenErr = nil
	wantToken(t, s, f, "token-3", 6)
}

// TestTokenLifetime: a token that lives less than two margins is replaced at half its life; one
// that has expired already, as moto's, is used for minLifetime, with one warning.
func TestTokenLifetime(t *testing.T) {
	c := newClock()
	f := newFakeECR(c)
	f.lifetime = 30 * time.Minute
	s := newTokens(f, slog.New(slog.DiscardHandler))
	wantToken(t, s, f, "token-1", 1)
	c.advance(15*time.Minute - time.Second)
	wantToken(t, s, f, "token-1", 1)
	c.advance(time.Second)
	wantToken(t, s, f, "token-2", 2)

	f.lifetime = -24 * time.Hour
	var logs bytes.Buffer
	s = newTokens(f, slog.New(slog.NewTextHandler(&logs, nil)))
	wantToken(t, s, f, "token-3", 3)
	c.advance(minLifetime/2 - time.Second)
	wantToken(t, s, f, "token-3", 3)
	c.advance(time.Second)
	wantToken(t, s, f, "token-4", 4)
	if n := strings.Count(logs.String(), "expires before it can be used"); n != 2 {
		t.Errorf("%d warnings about the expiry, want one per token:\n%s", n, logs.String())
	}
}

// TestTokenCredential: the credential of the data path is the current token; without one
// that works, the failure to get one is its error.
func TestTokenCredential(t *testing.T) {
	f := newFakeECR(newClock())
	s := newTokens(f, slog.New(slog.DiscardHandler))
	cred, err := s.credential(t.Context(), "123456789012.dkr.ecr.us-east-1.amazonaws.com")
	if err != nil || cred.Username != "AWS" || cred.Password != "token-1" {
		t.Errorf("credential = %+v, %v; want AWS:token-1", cred, err)
	}
	f.clock.advance(12 * time.Hour)
	f.tokenErr = &smithy.GenericAPIError{Code: "AccessDeniedException"}
	if _, err := s.credential(t.Context(), "123456789012.dkr.ecr.us-east-1.amazonaws.com"); !errors.Is(err, provider.ErrUnauthorized) {
		t.Errorf("credential without a token = %v, want ErrUnauthorized", err)
	}
}

// TestTokenConcurrent: requests that find no token wait for one GetAuthorizationToken.
func TestTokenConcurrent(t *testing.T) {
	f := newFakeECR(newClock())
	f.gate = make(chan struct{})
	s := newTokens(f, slog.New(slog.DiscardHandler))
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			_, err := s.get(t.Context())
			errs <- err
		})
	}
	close(f.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("get: %v", err)
		}
	}
	if got := f.count("GetAuthorizationToken"); got != 1 {
		t.Errorf("GetAuthorizationToken called %d times, want 1", got)
	}
}

func TestDecodeToken(t *testing.T) {
	enc := func(s string) *string { return aws.String(base64.StdEncoding.EncodeToString([]byte(s))) }
	for _, s := range []*string{nil, aws.String("not base64 s3cr3t!"), enc("no-colon-s3cr3t"), enc(":s3cr3t"), enc("AWS:")} {
		_, err := decodeToken(s)
		if err == nil {
			t.Errorf("decodeToken(%v) succeeded", aws.ToString(s))
		} else if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("the error shows the token: %v", err)
		}
	}
	got, err := decodeToken(enc("AWS:pass:with:colons"))
	if err != nil || got.Username != "AWS" || got.Password != "pass:with:colons" {
		t.Errorf("decodeToken = %+v, %v", got, err)
	}
}
