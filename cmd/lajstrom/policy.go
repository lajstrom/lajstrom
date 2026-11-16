package main

import (
	"context"
	"encoding/pem"
	"errors"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/policy"
	"github.com/lajstrom/lajstrom/internal/server"
)

// versionOrder returns the order of a format's versions for the ranges of a policy rule: the
// plugin's when it is a format.VersionComparer, nil otherwise.
func versionOrder(lookup func(string) (format.Plugin, bool)) func(string) policy.VersionOrder {
	return func(name string) policy.VersionOrder {
		p, ok := lookup(name)
		if !ok {
			return nil
		}
		if vc, ok := p.(format.VersionComparer); ok {
			return vc.CompareVersions
		}
		return nil
	}
}

// requestOrigin is the client's address and the request id of ctx, for the audit row of a
// refusal.
func requestOrigin(ctx context.Context) (addr, requestID string) {
	return server.ClientAddr(ctx), server.RequestID(ctx)
}

// policyKey returns the server's policy key, made now when the index has none (docs/spec/
// policy.md §5), through signer, the keys service of serve; nil without a master key (signer
// nil), with one line saying what the server does not sign then. A key that cannot be had is
// logged at error, and serve runs without it.
func policyKey(ctx context.Context, c *config.Config, signer format.Signer, log *slog.Logger) *keys.PolicyKey {
	svc, _ := signer.(*keys.Service)
	if svc == nil {
		log.InfoContext(ctx, "no policy key without a master key: the server signs no verdict and no deny list")
		return nil
	}
	k, err := svc.PolicyKey(ctx, c.Policy.SigningKey)
	if err != nil {
		log.ErrorContext(ctx, "the policy key cannot be had: the server signs no verdict and no deny list", "error", err.Error())
		return nil
	}
	log.InfoContext(ctx, "verdicts and the deny list are signed with the policy key", "fingerprint", k.Fingerprint(),
		"policy.signingKey", c.Policy.SigningKey)
	return k
}

// policyTrust returns the trust entry of the policy key that the index holds, the name lajstrom
// for the predicate type of verdicts (docs/spec/manifest-profile.md §6.3), from its public key
// alone: reindex labels the server's verdicts without the master key. None when there is no key.
func policyTrust(ctx context.Context, st index.Reader) ([]attest.Entry, error) {
	k, err := st.GetKey(ctx, nil, keys.KindPolicy)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return []attest.Entry{{Name: "lajstrom", PublicKey: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.Public}),
		PredicateTypes: []string{policy.PredicateType}}}, nil
}

// liftedBy returns the proxy core's Lifted of serve: the allow verdict of a quarantine over,
// signed with key, attached by the pipeline as the principal of the fetch; nil without a key.
func liftedBy(pipe *ingest.Pipeline, key *keys.PolicyKey) func(context.Context, index.Repo, index.FileMatch) {
	if key == nil {
		return nil
	}
	return func(ctx context.Context, repo index.Repo, m index.FileMatch) {
		pipe.LiftQuarantine(ctx, repo, m, key, auth.PrincipalFrom(ctx))
	}
}

// serveTrust returns the policy key of serve (policyKey) and the trust of attachments with it
// (policyTrust, attachmentTrust).
func serveTrust(ctx context.Context, c *config.Config, st index.Store, signer format.Signer, log *slog.Logger) (*keys.PolicyKey, *attest.Trust, error) {
	key := policyKey(ctx, c, signer, log)
	entries, err := policyTrust(ctx, st)
	if err != nil {
		return nil, nil, err
	}
	trust, err := attachmentTrust(c.Attachments, log, entries...)
	return key, trust, err
}

// newRules returns the service of the policy rules over st, for serve and lajstrom admin policy.
func newRules(c *config.Config, st index.Store, log *slog.Logger) *api.Rules {
	lookup := lookupIn(servedPlugins(c.Server.ExternalURL, nil))
	served := func(name string) bool {
		_, ok := lookup(name)
		return ok || name == "oci"
	}
	return &api.Rules{Index: st, Formats: served, Order: versionOrder(lookup), Webhooks: newOutbox(c), Log: log}
}

// apiPolicyKey is key as the API takes it: nil, not a nil *keys.PolicyKey, without one.
func apiPolicyKey(key *keys.PolicyKey) api.PolicyKey {
	if key == nil {
		return nil
	}
	return key
}
