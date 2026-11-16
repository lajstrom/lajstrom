package main

import (
	"context"

	"github.com/lajstrom/lajstrom/internal/format"
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
