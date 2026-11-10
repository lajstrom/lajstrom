// Package ecr is the Amazon ECR Provider: the generic provider's data path for /v2, with ECR's
// authorization token as its credentials, and the ECR API for what /v2 lacks.
//
// It registers the registries[].type "ecr" with provider.Register; cmd/lajstrom imports it for
// that. The credentials are the AWS SDK's default chain (environment, shared profile, web
// identity as with IRSA, the instance or task role), and registries[].ecr.endpoint replaces
// the API's endpoint (a VPC endpoint, moto in the tests). GetAuthorizationToken gives the
// Basic credentials of /v2; the token is cached and replaced an hour before its 12-hour
// expiry, and every request sends the current one. EnsureRepository creates a repository
// with CreateRepository, with the tag mutability and encryption of the configuration;
// ListRepositories is DescribeRepositories, filtered by the instance prefix; DeleteRepository
// honours registries[].ecr.forceDelete. Errors of the ECR API wrap the sentinels of package
// provider where one applies (translate). The SDK retries throttling with backoff.
//
// Tested against moto for the API and distribution for /v2, not against ECR itself:
// docs/registries/ecr.md says what was and what waits for the live check (#19).
package ecr
