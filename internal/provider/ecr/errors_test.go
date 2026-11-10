package ecr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/smithy-go"

	"github.com/lajstrom/lajstrom/internal/provider"
)

func TestTranslate(t *testing.T) {
	// ECR's messages name the repository with the instance prefix and the account.
	msg := aws.String("The repository with name 'art/raw/x' in registry with id '123456789012' ...")
	tests := []struct {
		name string
		err  error
		want error // a sentinel, or nil for none
	}{
		{"repository not found", &types.RepositoryNotFoundException{Message: msg}, provider.ErrNotFound},
		{"image not found", &types.ImageNotFoundException{Message: msg}, provider.ErrNotFound},
		{"immutable tag", &types.ImageTagAlreadyExistsException{Message: msg}, provider.ErrConflict},
		{"repository not empty", &types.RepositoryNotEmptyException{Message: msg}, provider.ErrConflict},
		{"quota", &types.LimitExceededException{Message: msg}, provider.ErrConflict},
		{"access denied", &smithy.GenericAPIError{Code: "AccessDeniedException", Message: *msg}, provider.ErrUnauthorized},
		{"bad signature", &smithy.GenericAPIError{Code: "InvalidSignatureException", Message: *msg}, provider.ErrUnauthorized},
		{"throttled", &smithy.GenericAPIError{Code: "ThrottlingException", Message: "Rate exceeded"}, nil},
		{"server", &types.ServerException{Message: aws.String("internal")}, nil},
		{"canceled", fmt.Errorf("operation error ECR: %w", context.Canceled), nil},
	}
	sentinels := []error{provider.ErrNotFound, provider.ErrConflict, provider.ErrUnauthorized, provider.ErrUnsupported}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := translate("create repository raw/x", fmt.Errorf("operation error ECR: CreateRepository: %w", tt.err))
			for _, s := range sentinels {
				if want := tt.want != nil && errors.Is(tt.want, s); errors.Is(err, s) != want {
					t.Errorf("errors.Is(%v, %v) = %v", err, s, !want)
				}
			}
			if !errors.Is(err, tt.err) {
				t.Errorf("%v does not wrap the cause", err)
			}
			if !strings.HasPrefix(err.Error(), "ecr: create repository raw/x: ") {
				t.Errorf("message %q does not name the operation", err)
			}
			if tt.want != nil && (strings.Contains(err.Error(), "art/") || strings.Contains(err.Error(), "123456789012")) {
				t.Errorf("message %q shows the instance prefix or the account", err)
			}
		})
	}
	if translate("x", nil) != nil {
		t.Error("translate(nil) is not nil")
	}
}

func TestRetryer(t *testing.T) {
	tests := []struct {
		retries, attempts int
	}{{0, 4}, {1, 2}, {-1, 1}}
	for _, tt := range tests {
		if got := retryer(tt.retries)().MaxAttempts(); got != tt.attempts {
			t.Errorf("retries %d: MaxAttempts = %d, want %d", tt.retries, got, tt.attempts)
		}
	}
	r := retryer(0)()
	for code, want := range map[string]bool{
		"ThrottlingException": true, "TooManyRequestsException": true, "RequestTimeout": true,
		"LimitExceededException": false, "AccessDeniedException": false, "RepositoryNotFoundException": false,
	} {
		if got := r.IsErrorRetryable(&smithy.GenericAPIError{Code: code}); got != want {
			t.Errorf("%s retryable = %v, want %v", code, got, want)
		}
	}
}
