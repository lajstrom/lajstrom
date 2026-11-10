package ecr

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/config"
)

// TestCapabilityDefaults: ECR's capabilities fill what registries[].capabilities leaves unset,
// and immutableTags follows the mutability of the repositories Lajstrom creates.
func TestCapabilityDefaults(t *testing.T) {
	tests := []struct {
		name       string
		set        config.CapabilityOverrides
		mutability types.ImageTagMutability
		want       config.CapabilityOverrides
	}{
		{"defaults", config.CapabilityOverrides{}, types.ImageTagMutabilityImmutable,
			config.CapabilityOverrides{CreateOnPush: aws.Bool(false), Catalog: aws.Bool(true), ImmutableTags: aws.Bool(true), MaxBlobSize: maxLayerSize}},
		{"exclusion", config.CapabilityOverrides{}, types.ImageTagMutabilityImmutableWithExclusion,
			config.CapabilityOverrides{CreateOnPush: aws.Bool(false), Catalog: aws.Bool(true), ImmutableTags: aws.Bool(true), MaxBlobSize: maxLayerSize}},
		{"mutable", config.CapabilityOverrides{}, types.ImageTagMutabilityMutableWithExclusion,
			config.CapabilityOverrides{CreateOnPush: aws.Bool(false), Catalog: aws.Bool(true), ImmutableTags: aws.Bool(false), MaxBlobSize: maxLayerSize}},
		{"set", config.CapabilityOverrides{CreateOnPush: aws.Bool(true), Catalog: aws.Bool(false), Referrers: aws.Bool(false), MaxBlobSize: 1 << 30},
			types.ImageTagMutabilityImmutable,
			config.CapabilityOverrides{CreateOnPush: aws.Bool(true), Catalog: aws.Bool(false), Referrers: aws.Bool(false), ImmutableTags: aws.Bool(true), MaxBlobSize: 1 << 30}},
	}
	for _, tt := range tests {
		if diff := cmp.Diff(tt.want, capabilities(tt.set, tt.mutability)); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tt.name, diff)
		}
	}
}
