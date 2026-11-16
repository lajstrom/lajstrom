// Package policy decides which artifacts a repo serves (docs/spec/policy.md, ADR 0008): rules
// over coordinates from the control plane and verdicts, signed attestations about one manifest,
// from the index. The protocol frontends, ingest, the proxy core and the facade ask its Decider;
// a render reads the index through a View, which leaves out what it refuses.
//
// It imports index, auth and config and nothing of the packages that ask it, so that format can
// import it: the order of a format's versions comes in as Options.Order.
package policy
