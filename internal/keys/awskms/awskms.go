// Package awskms is the AWS KMS of internal/keys (ADR 0020): the calls DescribeKey,
// GetPublicKey and Sign of the KMS JSON 1.1 API over net/http, signed with SigV4 by
// aws-sdk-go-v2, with the credentials of the SDK's default chain, as the ECR adapter gets them.
// It holds no key material: KMS keys never leave the KMS.
package awskms

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/lajstrom/lajstrom/internal/keys"
)

const (
	// DefaultTimeout bounds one request to AWS KMS.
	DefaultTimeout = 10 * time.Second
	// DefaultRetries is how many times a request that met an unavailable KMS is sent again.
	DefaultRetries = 2

	// The signing algorithms of OpenPGP signatures by an RSA and a P-256 key.
	algorithmRSA   = "RSASSA_PKCS1_V1_5_SHA_256"
	algorithmECDSA = "ECDSA_SHA_256"

	// maxAnswer bounds what is read of an answer: a public key of RSA 4096 is under 1 KiB.
	maxAnswer = 64 << 10
	// maxMessage bounds the message of a KMS error that goes into an error.
	maxMessage = 300
)

// keySpecs are the key specs of AWS KMS that sign as internal/keys needs, and their algorithm.
var keySpecs = map[string]string{
	"RSA_2048": algorithmRSA, "RSA_3072": algorithmRSA, "RSA_4096": algorithmRSA, "ECC_NIST_P256": algorithmECDSA,
}

// unavailableTypes are the error types of AWS KMS that a later request may not meet: the KMS
// throttled, failed, or did not reach the key's HSM in time.
var unavailableTypes = map[string]bool{
	"ThrottlingException": true, "KMSInternalException": true, "DependencyTimeoutException": true,
	"KeyUnavailableException": true, "ServiceUnavailableException": true, "InternalFailure": true,
	"ServiceUnavailable": true, "RequestTimeoutException": true,
}

// Options configure a Client.
type Options struct {
	// Endpoint is the base URL of AWS KMS for every region: AWS_ENDPOINT_URL_KMS, else
	// AWS_ENDPOINT_URL, for a VPC endpoint, FIPS or an emulator. Empty: https://kms.<region>.
	// and the domain of the region's partition.
	Endpoint string
	// Region is the region of a key named without its ARN; empty: the SDK's (AWS_REGION, the
	// shared configuration). A key ARN names its own.
	Region string
	// Timeout bounds one request; zero is DefaultTimeout.
	Timeout time.Duration
	// Retries is how many times a request is sent again after the KMS was unavailable; zero is
	// DefaultRetries, negative none.
	Retries int
	// Credentials replaces the SDK's default chain; tests set it.
	Credentials aws.CredentialsProvider
	// HTTPClient replaces the client of the requests; tests set it.
	HTTPClient *http.Client
	// Log gets a line for each request sent again; nil is slog.Default().
	Log *slog.Logger
}

// Client is AWS KMS for internal/keys: it implements keys.KMS. The SDK's configuration is read at
// the first request, so a server that holds no key in AWS KMS never reads it. It is safe for
// concurrent use.
type Client struct {
	o       Options
	http    *http.Client
	signer  *v4.Signer
	log     *slog.Logger
	backoff time.Duration

	once   sync.Once
	creds  aws.CredentialsProvider
	region string
	err    error
}

var _ keys.KMS = (*Client)(nil)

// New returns the Client of o. It does not talk to AWS.
func New(o Options) *Client {
	o.Timeout = cmp.Or(o.Timeout, DefaultTimeout)
	switch {
	case o.Retries == 0:
		o.Retries = DefaultRetries
	case o.Retries < 0:
		o.Retries = 0
	}
	c := &Client{o: o, http: o.HTTPClient, signer: v4.NewSigner(), log: o.Log, backoff: 250 * time.Millisecond}
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

// config returns the credentials and the default region: Options.Credentials and Region, else
// the SDK's default chain, read once.
func (c *Client) config(ctx context.Context) (aws.CredentialsProvider, string, error) {
	c.once.Do(func() {
		c.creds, c.region = c.o.Credentials, c.o.Region
		if c.creds != nil {
			return
		}
		var opts []func(*awsconfig.LoadOptions) error
		if c.region != "" {
			opts = append(opts, awsconfig.WithRegion(c.region))
		}
		cfg, err := awsconfig.LoadDefaultConfig(context.WithoutCancel(ctx), opts...)
		if err != nil {
			c.err = fmt.Errorf("awskms: AWS configuration: %w", err)
			return
		}
		c.creds, c.region = cfg.Credentials, cfg.Region
	})
	return c.creds, c.region, c.err
}

// Key describes the key of name, a key id, key ARN, alias/<name> or alias ARN: DescribeKey for
// its ARN, usage, spec, state and creation date, then GetPublicKey by the ARN. A key that is not
// enabled, whose usage is not SIGN_VERIFY, or whose spec is not RSA_2048, RSA_3072, RSA_4096 or
// ECC_NIST_P256, is keys.ErrKMSKeyUnsupported.
func (c *Client) Key(ctx context.Context, name string) (*keys.KMSKey, error) {
	var desc struct {
		KeyMetadata struct {
			Arn               string
			CreationDate      float64
			KeyUsage          string
			KeySpec           string
			KeyState          string
			SigningAlgorithms []string
		}
	}
	if err := c.call(ctx, name, "DescribeKey", map[string]string{"KeyId": name}, &desc); err != nil {
		return nil, err
	}
	m := desc.KeyMetadata
	algorithm := keySpecs[m.KeySpec]
	switch {
	case m.Arn == "":
		return nil, errors.New("awskms: DescribeKey answered no key ARN")
	case m.KeyUsage != "SIGN_VERIFY":
		return nil, fmt.Errorf("%w: %s has the usage %s, not SIGN_VERIFY", keys.ErrKMSKeyUnsupported, m.Arn, m.KeyUsage)
	case algorithm == "":
		return nil, fmt.Errorf("%w: %s is a key of %s, not RSA_2048, RSA_3072, RSA_4096 or ECC_NIST_P256", keys.ErrKMSKeyUnsupported,
			m.Arn, m.KeySpec)
	case m.KeyState != "Enabled":
		return nil, fmt.Errorf("%w: %s is %s, not Enabled", keys.ErrKMSKeyUnsupported, m.Arn, m.KeyState)
	case !slices.Contains(m.SigningAlgorithms, algorithm):
		return nil, fmt.Errorf("%w: %s does not sign with %s", keys.ErrKMSKeyUnsupported, m.Arn, algorithm)
	case m.CreationDate <= 0 || m.CreationDate > math.MaxInt32:
		return nil, fmt.Errorf("awskms: DescribeKey answered the creation date %v", m.CreationDate)
	}
	var pub struct{ PublicKey []byte }
	if err := c.call(ctx, m.Arn, "GetPublicKey", map[string]string{"KeyId": m.Arn}, &pub); err != nil {
		return nil, err
	}
	key, err := x509.ParsePKIXPublicKey(pub.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("awskms: the public key of %s: %w", m.Arn, err)
	}
	return &keys.KMSKey{Name: m.Arn, Public: key, Created: time.Unix(int64(m.CreationDate), 0)}, nil
}

// Sign signs digest, a SHA-256 hash, with the key of the ARN name: RSASSA_PKCS1_V1_5_SHA_256 for
// an RSA key, ECDSA_SHA_256 for a P-256 key, with MessageType DIGEST.
func (c *Client) Sign(ctx context.Context, name string, pub crypto.PublicKey, digest []byte) ([]byte, error) {
	var algorithm string
	switch pub.(type) {
	case *rsa.PublicKey:
		algorithm = algorithmRSA
	case *ecdsa.PublicKey:
		algorithm = algorithmECDSA
	default:
		return nil, fmt.Errorf("%w: a public key of type %T", keys.ErrKMSKeyUnsupported, pub)
	}
	in := map[string]any{"KeyId": name, "Message": digest, "MessageType": "DIGEST", "SigningAlgorithm": algorithm}
	var out struct{ Signature []byte }
	if err := c.call(ctx, name, "Sign", in, &out); err != nil {
		return nil, err
	}
	if len(out.Signature) == 0 {
		return nil, errors.New("awskms: Sign answered no signature")
	}
	return out.Signature, nil
}

// call sends the request op of the KMS API with in for the key of name, whose ARN names its
// region, and decodes the answer into out; again after an unavailable KMS, up to Retries times.
func (c *Client) call(ctx context.Context, name, op string, in, out any) error {
	creds, region, err := c.config(ctx)
	if err != nil {
		return err
	}
	partition := ""
	if arn, ok := parseARN(name); ok {
		partition, region = arn.partition, arn.region
	}
	if region == "" {
		return fmt.Errorf("awskms: %s names no region: name the key by its ARN, or set AWS_REGION", name)
	}
	endpoint, err := endpointOf(c.o.Endpoint, partition, region)
	if err != nil {
		return err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("awskms: %s: %w", op, err)
	}
	wait := c.backoff
	for attempt := 0; ; attempt++ {
		err = c.send(ctx, creds, endpoint, region, op, body, out)
		if err == nil || !errors.Is(err, keys.ErrKMSUnavailable) || attempt >= c.o.Retries {
			return err
		}
		c.log.WarnContext(ctx, "AWS KMS unavailable, asking again", "op", op, "key", name, "attempt", attempt+1, "error", err.Error())
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait = min(2*wait, 2*time.Second)
	}
}

// send sends one request: SigV4 of the service kms in region, at endpoint.
func (c *Client) send(ctx context.Context, creds aws.CredentialsProvider, endpoint, region, op string, body []byte, out any) error {
	cred, err := creds.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("awskms: AWS credentials: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("awskms: %s: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService."+op)
	sum := sha256.Sum256(body)
	if err := c.signer.SignHTTP(ctx, cred, req, hex.EncodeToString(sum[:]), "kms", region, time.Now()); err != nil {
		return fmt.Errorf("awskms: sign the request %s: %w", op, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("awskms: %s: %w", op, err)
		}
		return fmt.Errorf("%w: AWS KMS %s: %w", keys.ErrKMSUnavailable, op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return fmt.Errorf("%w: AWS KMS %s: the answer: %w", keys.ErrKMSUnavailable, op, err)
	}
	if resp.StatusCode != http.StatusOK {
		return refusal(op, resp, answer)
	}
	if len(bytes.TrimSpace(answer)) == 0 { // CreateAlias answers nothing; the callers check what they need
		return nil
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("awskms: %s answered what is not its JSON: %w", op, err)
	}
	return nil
}

// refusal is the error of an answer that is not 200: keys.ErrKMSUnavailable for a 5xx, a 429 or
// a type of unavailableTypes, a plain error otherwise, with the KMS's type and message.
func refusal(op string, resp *http.Response, answer []byte) error {
	var e struct {
		Type     string `json:"__type"`
		Message  string `json:"message"`
		Message2 string `json:"Message"`
	}
	_ = json.Unmarshal(answer, &e) // a body that is not JSON leaves the status alone
	typ := cmp.Or(e.Type, resp.Header.Get("X-Amzn-ErrorType"))
	typ, _, _ = strings.Cut(typ, ":")
	if i := strings.LastIndexByte(typ, '#'); i >= 0 {
		typ = typ[i+1:]
	}
	msg := cmp.Or(e.Message, e.Message2)
	if len(msg) > maxMessage {
		msg = msg[:maxMessage] + "…"
	}
	err := fmt.Errorf("AWS KMS %s answered %d %s: %s", op, resp.StatusCode, cmp.Or(typ, "without an error type"), msg)
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || unavailableTypes[typ] {
		return fmt.Errorf("%w: %w", keys.ErrKMSUnavailable, err)
	}
	return fmt.Errorf("awskms: %w", err)
}

// arn is what a KMS ARN names: arn:<partition>:kms:<region>:<account>:key/<id> or alias/<name>.
type arn struct{ partition, region string }

// parseARN reads the partition and the region of a KMS ARN; false for a name that is not one.
func parseARN(name string) (arn, bool) {
	parts := strings.SplitN(name, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "kms" || parts[1] == "" || parts[3] == "" {
		return arn{}, false
	}
	return arn{partition: parts[1], region: parts[3]}, true
}

// partitionDomains are the domains of the partitions whose KMS endpoint is
// kms.<region>.<domain>; another partition needs AWS_ENDPOINT_URL_KMS.
var partitionDomains = map[string]string{"aws": "amazonaws.com", "aws-cn": "amazonaws.com.cn", "aws-us-gov": "amazonaws.com"}

// endpointOf returns the URL that requests for a key of the partition ("" when the key is not
// named by its ARN: the region's) and region go to: base when it is set, else the KMS endpoint
// of the region.
func endpointOf(base, partition, region string) (string, error) {
	if base != "" {
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return "", fmt.Errorf("awskms: the endpoint %q of AWS_ENDPOINT_URL_KMS or AWS_ENDPOINT_URL is not an http(s) URL", base)
		}
		return strings.TrimSuffix(u.String(), "/") + "/", nil
	}
	if partition == "" {
		switch {
		case strings.HasPrefix(region, "cn-"):
			partition = "aws-cn"
		case strings.HasPrefix(region, "us-gov-"):
			partition = "aws-us-gov"
		default:
			partition = "aws"
		}
	}
	domain, ok := partitionDomains[partition]
	if !ok {
		return "", fmt.Errorf("awskms: the partition %s has no endpoint known here: set AWS_ENDPOINT_URL_KMS", partition)
	}
	if strings.ContainsFunc(region, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' }) {
		return "", fmt.Errorf("awskms: %q is not a region", region)
	}
	return "https://kms." + region + "." + domain + "/", nil
}
