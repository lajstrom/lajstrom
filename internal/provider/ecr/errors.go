package ecr

import (
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// apiError is an error of the ECR API translated into a sentinel of package provider. Its
// message names the operation, the Lajstrom name and the ECR error code, and leaves out ECR's
// own message, which names the repository with the instance prefix and the account:
// format.WriteError puts the message of a 4xx error in the response. errors.As still finds the
// smithy.APIError.
type apiError struct {
	op    string // "create repository raw/scratch/notes"
	kind  error  // a sentinel of package provider
	code  string // "RepositoryNotFoundException"
	cause error
}

func (e *apiError) Error() string {
	return "ecr: " + e.op + ": " + e.kind.Error() + " (" + e.code + ")"
}

func (e *apiError) Unwrap() []error { return []error{e.kind, e.cause} }

// sentinels maps the error codes of the ECR API to the sentinels of package provider; a code
// that is not here is wrapped as it is (500).
var sentinels = map[string]error{
	"RepositoryNotFoundException": provider.ErrNotFound,
	"ImageNotFoundException":      provider.ErrNotFound,
	"LayersNotFoundException":     provider.ErrNotFound,
	"UploadNotFoundException":     provider.ErrNotFound,

	// ECR refuses the write because of what it holds: a tag of an immutable repository, a
	// repository that is not empty, or a quota (repositories per region, images per
	// repository) that what it holds has reached. A quota is no fault of the credentials, and
	// retrying does not help: 409, with the code in the message. To be measured by #19: which
	// code a push over the image quota gets on /v2.
	"ImageTagAlreadyExistsException":   provider.ErrConflict,
	"ImageAlreadyExistsException":      provider.ErrConflict,
	"RepositoryAlreadyExistsException": provider.ErrConflict,
	"RepositoryNotEmptyException":      provider.ErrConflict,
	"LimitExceededException":           provider.ErrConflict,

	// ECR or IAM rejected Lajstrom's own credentials, or what they may do.
	"AccessDeniedException":       provider.ErrUnauthorized,
	"UnrecognizedClientException": provider.ErrUnauthorized,
	"InvalidSignatureException":   provider.ErrUnauthorized,
	"ExpiredTokenException":       provider.ErrUnauthorized,
	"MissingAuthenticationToken":  provider.ErrUnauthorized,
}

// translate turns an error of the ECR API into one that wraps a sentinel of package provider
// where sentinels has its code. Everything else, throttling that outlasted the SDK's retries
// and context errors included, is wrapped with %w and the operation. nil stays nil.
func translate(op string, err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		if kind, ok := sentinels[api.ErrorCode()]; ok {
			return &apiError{op: op, kind: kind, code: api.ErrorCode(), cause: err}
		}
	}
	return fmt.Errorf("ecr: %s: %w", op, err)
}

// isCode reports whether err is an error of the ECR API with the code.
func isCode(err error, code string) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == code
}

// backoff replaces the SDK's backoff between retries when it is set: the tests do not wait.
var backoff retry.BackoffDelayer

// retryer is the SDK's standard retryer with registries[].retries (0 is the default, negative
// none): throttling, 5xx answers and connection errors are retried with its jittered backoff.
// LimitExceededException is not: ECR answers it for a quota, not for a rate.
func retryer(retries int) func() aws.Retryer {
	switch {
	case retries == 0:
		retries = config.DefaultRegistryRetries
	case retries < 0:
		retries = 0
	}
	quota := retry.IsErrorRetryableFunc(func(err error) aws.Ternary {
		if isCode(err, "LimitExceededException") {
			return aws.FalseTernary
		}
		return aws.UnknownTernary
	})
	return func() aws.Retryer {
		return retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = retries + 1
			o.Retryables = append([]retry.IsErrorRetryable{quota}, o.Retryables...)
			if backoff != nil {
				o.Backoff = backoff
			}
		})
	}
}
