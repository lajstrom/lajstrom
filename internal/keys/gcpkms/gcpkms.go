// Package gcpkms is the GCP Cloud KMS of internal/keys (ADR 0020): cryptoKeyVersions.get,
// getPublicKey and asymmetricSign of the Cloud KMS REST API over net/http, with the access token
// of the workload's service account from the metadata server (internal/gcpauth), as an oci
// upstream with auth.gcp gets it. It holds no key material: KMS keys never leave the KMS.
package gcpkms

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/gcpauth"
	"github.com/lajstrom/lajstrom/internal/keys"
)

const (
	// DefaultEndpoint is the global endpoint of Cloud KMS.
	DefaultEndpoint = "https://cloudkms.googleapis.com"
	// DefaultTimeout bounds one request to Cloud KMS.
	DefaultTimeout = 10 * time.Second
	// DefaultRetries is how many times a request that met an unavailable KMS is sent again.
	DefaultRetries = 2

	// maxAnswer bounds what is read of an answer: a PEM public key of RSA 4096 is under 1 KiB.
	maxAnswer = 64 << 10
	// maxMessage bounds the message of a KMS error that goes into an error.
	maxMessage = 300
)

// algorithms are the algorithms of Cloud KMS key versions that sign as internal/keys needs:
// PKCS #1 v1.5 or ECDSA, with SHA-256.
var algorithms = map[string]bool{
	"RSA_SIGN_PKCS1_2048_SHA256": true, "RSA_SIGN_PKCS1_3072_SHA256": true, "RSA_SIGN_PKCS1_4096_SHA256": true,
	"EC_SIGN_P256_SHA256": true,
}

// unavailableStatuses are the canonical statuses of a Google API error that a later request may
// not meet.
var unavailableStatuses = map[string]bool{"UNAVAILABLE": true, "RESOURCE_EXHAUSTED": true, "INTERNAL": true, "DEADLINE_EXCEEDED": true}

// castagnoli is the CRC32C of Cloud KMS's integrity fields.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Options configure a Client.
type Options struct {
	// Endpoint is the base URL of Cloud KMS; empty is DefaultEndpoint.
	Endpoint string
	// Token returns the access token of a request; nil is the metadata server at MetadataHost,
	// made at the first request.
	Token func(context.Context) (string, error)
	// MetadataHost is the host of the metadata server; empty is GCE_METADATA_HOST, else
	// 169.254.169.254 (internal/gcpauth).
	MetadataHost string
	// Timeout bounds one request; zero is DefaultTimeout.
	Timeout time.Duration
	// Retries is how many times a request is sent again after the KMS was unavailable; zero is
	// DefaultRetries, negative none.
	Retries int
	// HTTPClient replaces the client of the requests; tests set it.
	HTTPClient *http.Client
	// Log gets a line for each request sent again; nil is slog.Default().
	Log *slog.Logger
}

// Client is GCP Cloud KMS for internal/keys: it implements keys.KMS. It is safe for concurrent
// use.
type Client struct {
	o       Options
	http    *http.Client
	log     *slog.Logger
	backoff time.Duration

	once  sync.Once
	token func(context.Context) (string, error)
	err   error
}

var _ keys.KMS = (*Client)(nil)

// New returns the Client of o. It does not talk to GCP.
func New(o Options) *Client {
	o.Endpoint = strings.TrimSuffix(cmp.Or(o.Endpoint, DefaultEndpoint), "/")
	o.Timeout = cmp.Or(o.Timeout, DefaultTimeout)
	switch {
	case o.Retries == 0:
		o.Retries = DefaultRetries
	case o.Retries < 0:
		o.Retries = 0
	}
	c := &Client{o: o, http: o.HTTPClient, log: o.Log, backoff: 250 * time.Millisecond}
	if c.http == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = o.Timeout
		c.http = &http.Client{Transport: t, Timeout: o.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c
}

// tokenSource returns Options.Token, or the metadata server's tokens, made once.
func (c *Client) tokenSource() (func(context.Context) (string, error), error) {
	c.once.Do(func() {
		if c.o.Token != nil {
			c.token = c.o.Token
			return
		}
		t, err := gcpauth.New(cmp.Or(c.o.MetadataHost, os.Getenv(gcpauth.MetadataHostEnv)), 0, 0, c.log)
		if err != nil {
			c.err = fmt.Errorf("gcpkms: %w", err)
			return
		}
		c.token = t.Token
	})
	return c.token, c.err
}

// Key describes the key version of name: its state, algorithm and creation time, then its public
// key. A version that is not ENABLED, or whose algorithm is not RSA_SIGN_PKCS1_2048_SHA256,
// RSA_SIGN_PKCS1_3072_SHA256, RSA_SIGN_PKCS1_4096_SHA256 or EC_SIGN_P256_SHA256, is
// keys.ErrKMSKeyUnsupported.
func (c *Client) Key(ctx context.Context, name string) (*keys.KMSKey, error) {
	var v struct {
		Name       string
		State      string
		Algorithm  string
		CreateTime string
	}
	if err := c.call(ctx, http.MethodGet, name, "", nil, &v); err != nil {
		return nil, err
	}
	switch {
	case !algorithms[v.Algorithm]:
		return nil, fmt.Errorf("%w: %s is a version of %s, not RSA_SIGN_PKCS1_{2048,3072,4096}_SHA256 or EC_SIGN_P256_SHA256",
			keys.ErrKMSKeyUnsupported, name, cmp.Or(v.Algorithm, "no algorithm"))
	case v.State != "ENABLED":
		return nil, fmt.Errorf("%w: %s is %s, not ENABLED", keys.ErrKMSKeyUnsupported, name, v.State)
	}
	created, err := time.Parse(time.RFC3339Nano, v.CreateTime)
	if err != nil {
		return nil, fmt.Errorf("gcpkms: the createTime of %s: %w", name, err)
	}
	var pub struct {
		Pem       string
		PemCrc32c string
	}
	if err := c.call(ctx, http.MethodGet, name, "/publicKey", nil, &pub); err != nil {
		return nil, err
	}
	if err := checkCRC([]byte(pub.Pem), pub.PemCrc32c); err != nil {
		return nil, fmt.Errorf("gcpkms: the public key of %s: %w", name, err)
	}
	block, _ := pem.Decode([]byte(pub.Pem))
	if block == nil {
		return nil, fmt.Errorf("gcpkms: the public key of %s is not PEM", name)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("gcpkms: the public key of %s: %w", name, err)
	}
	return &keys.KMSKey{Name: name, Public: key, Created: created}, nil
}

// Sign signs digest, a SHA-256 hash, with the key version of name, whose algorithm is the
// version's own; the KMS checks the digest's CRC32C and Sign the signature's.
func (c *Client) Sign(ctx context.Context, name string, _ crypto.PublicKey, digest []byte) ([]byte, error) {
	in := map[string]any{"digest": map[string][]byte{"sha256": digest},
		"digestCrc32c": strconv.FormatUint(uint64(crc32.Checksum(digest, castagnoli)), 10)}
	var out struct {
		Name                 string
		Signature            []byte
		SignatureCrc32c      string
		VerifiedDigestCrc32c bool
	}
	if err := c.call(ctx, http.MethodPost, name, ":asymmetricSign", in, &out); err != nil {
		return nil, err
	}
	switch {
	case !out.VerifiedDigestCrc32c:
		return nil, fmt.Errorf("%w: Cloud KMS did not verify the digest's CRC32C", keys.ErrKMSUnavailable)
	case len(out.Signature) == 0:
		return nil, errors.New("gcpkms: asymmetricSign answered no signature")
	}
	if err := checkCRC(out.Signature, out.SignatureCrc32c); err != nil {
		return nil, fmt.Errorf("%w: the signature: %w", keys.ErrKMSUnavailable, err)
	}
	return out.Signature, nil
}

// checkCRC checks that b has the CRC32C sum, a decimal integer as Cloud KMS writes an int64.
func checkCRC(b []byte, sum string) error {
	if sum == "" {
		return nil // an emulator may leave it out; the signature is checked with the public key anyway
	}
	want, err := strconv.ParseUint(sum, 10, 32)
	if err != nil || uint32(want) != crc32.Checksum(b, castagnoli) {
		return fmt.Errorf("the CRC32C %s is not that of the bytes", sum)
	}
	return nil
}

// call sends the request method of the resource name with suffix (a sub-resource or a custom
// method) and the JSON in (nil for none), and decodes the answer into out; again after an
// unavailable KMS, up to Retries times.
func (c *Client) call(ctx context.Context, method, name, suffix string, in, out any) error {
	token, err := c.tokenSource()
	if err != nil {
		return err
	}
	segments := strings.Split(name, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	target := c.o.Endpoint + "/v1/" + strings.Join(segments, "/") + suffix
	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("gcpkms: %w", err)
		}
	}
	wait := c.backoff
	for attempt := 0; ; attempt++ {
		err = c.send(ctx, token, method, target, body, out)
		if err == nil || !errors.Is(err, keys.ErrKMSUnavailable) || attempt >= c.o.Retries {
			return err
		}
		c.log.WarnContext(ctx, "Cloud KMS unavailable, asking again", "key", name, "attempt", attempt+1, "error", err.Error())
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait = min(2*wait, 2*time.Second)
	}
}

// send sends one request with the access token of token.
func (c *Client) send(ctx context.Context, token func(context.Context) (string, error), method, target string, body []byte, out any) error {
	tok, err := token(ctx)
	if err != nil {
		// The metadata server is on the node; it fails when it is restarting, rarely for good.
		return fmt.Errorf("%w: the access token: %w", keys.ErrKMSUnavailable, err)
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, r)
	if err != nil {
		return fmt.Errorf("gcpkms: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("gcpkms: %w", err)
		}
		return fmt.Errorf("%w: Cloud KMS: %w", keys.ErrKMSUnavailable, withoutURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return fmt.Errorf("%w: Cloud KMS: the answer: %w", keys.ErrKMSUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return refusal(resp.StatusCode, answer)
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("gcpkms: Cloud KMS answered what is not its JSON: %w", err)
	}
	return nil
}

// withoutURL is err without the *url.Error's URL, which names the key's resource: the cause
// is enough, and the key is named by the caller.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// refusal is the error of an answer that is not 200: keys.ErrKMSUnavailable for a 5xx, a 429 or
// a status of unavailableStatuses, a plain error otherwise, with the API's status and message.
func refusal(status int, answer []byte) error {
	var e struct {
		Error struct {
			Message string
			Status  string
		}
	}
	_ = json.Unmarshal(answer, &e) // a body that is not JSON leaves the status alone
	msg := e.Error.Message
	if len(msg) > maxMessage {
		msg = msg[:maxMessage] + "…"
	}
	detail := fmt.Sprintf("Cloud KMS answered %d %s: %s", status, cmp.Or(e.Error.Status, http.StatusText(status)), msg)
	if status >= 500 || status == http.StatusTooManyRequests || unavailableStatuses[e.Error.Status] {
		return fmt.Errorf("%w: %s", keys.ErrKMSUnavailable, detail)
	}
	return errors.New("gcpkms: " + detail)
}
