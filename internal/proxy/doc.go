// Package proxy is the format-independent core of proxy repos (ADR 0013,
// docs/spec/proxy-and-group.md): a proxy repo serves an upstream that the configuration names,
// keeps the upstream's documents in proxy_meta as a cache, and fetches a file it does not hold
// on its first request, into the one write path, so that what it fetched is an ordinary
// manifest in the registry.
//
// The format's plugin provides the hooks (format.Proxier); this package does the rest: the
// settings of a proxy repo and its allow and deny lists, the client of an upstream, the
// metadata cache with its TTLs, negative entries and offline mode, and the fetch on a miss with
// its claim across replicas, spool, checksum and ingest.
package proxy
