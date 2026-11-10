// Package metrics holds the Prometheus collectors of Lajstrom and the handler of the metrics
// listener (docs/spec/http-routing.md §5; docs/operations.md, Metrics). The collectors are
// package values in one registry of their own, so that every package records into them without
// plumbing; the registry holds nothing else but the Go and process collectors. Every name
// starts with lajstrom_, and no label carries a repo or package name: the labels are route
// patterns, methods, statuses, formats, registry names and operations, so the number of series
// is fixed by the code and the configuration, not by the content.
package metrics

import (
	"net/http"
	"net/http/pprof"
	"runtime"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// sizeBuckets are the buckets of the size histograms: 256 B to 4 GiB by factors of 4.
var sizeBuckets = prometheus.ExponentialBuckets(256, 4, 13)

var registry = prometheus.NewRegistry()

var (
	// HTTPDuration is the time of a request on the main listener, from the first middleware to
	// the end of the handler, by method, route pattern and status.
	HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "lajstrom_http_request_duration_seconds", Help: "Time to answer an HTTP request, by route pattern.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route", "status"})
	// HTTPRequestSize is the size of a request body as declared, by method, route and status.
	HTTPRequestSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "lajstrom_http_request_size_bytes", Help: "Content-Length of the HTTP requests; -1 (unknown) is not counted.",
		Buckets: sizeBuckets,
	}, []string{"method", "route", "status"})
	// HTTPResponseSize is the size of a response body as written, by method, route and status.
	HTTPResponseSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "lajstrom_http_response_size_bytes", Help: "Bytes written in the HTTP response bodies.",
		Buckets: sizeBuckets,
	}, []string{"method", "route", "status"})
	// ProviderDuration is the time of a request to a backend registry, by registry name, operation
	// (manifest, blob, upload, tags, catalog, referrers, ping, token) and status ("error" when
	// there was no answer).
	ProviderDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "lajstrom_provider_request_duration_seconds", Help: "Time of a request to a backend registry, to its response headers.",
		Buckets: prometheus.DefBuckets,
	}, []string{"registry", "operation", "method", "status"})
	// ProviderCircuitOpen is 1 while the circuit breaker of a registry is open or half-open:
	// its requests fail at once instead of waiting for timeouts and retries.
	ProviderCircuitOpen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lajstrom_provider_circuit_open", Help: "1 while the circuit breaker of a backend registry is open.",
	}, []string{"registry"})
	// DBDuration is the time of a statement of the index database, by the store method that ran
	// it and whether it failed.
	DBDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "lajstrom_db_query_duration_seconds", Help: "Time of a statement of the index database, by store method.",
		Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"method", "result"})
	// Ingest counts the publishes through the ingest pipeline by format and result: published,
	// existed (an idempotent repeat), invalid, conflict, error.
	Ingest = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lajstrom_ingest_total", Help: "Publishes through the ingest pipeline, by format and result.",
	}, []string{"format", "result"})
	// ProxyUpstreamDuration is the time of a request to a proxy repo's upstream, to its
	// response headers, by upstream name (upstreams[].name), operation (document, file,
	// forward: a request of a client's sent on, npm audit) and status ("error" when there was no
	// answer).
	ProxyUpstreamDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "lajstrom_proxy_upstream_request_duration_seconds", Help: "Time of a request to a proxy's upstream, to its response headers.",
		Buckets: prometheus.DefBuckets,
	}, []string{"upstream", "operation", "status"})
	// ProxyDocuments counts the reads of an upstream's documents by a proxy repo, by upstream and
	// result: fresh (the cache, no request), revalidated (304), refreshed (200), absent (404 or
	// 410), stale (the upstream failed and the cache was used), offline (no request), error.
	ProxyDocuments = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lajstrom_proxy_documents_total", Help: "Reads of upstream documents by proxy repos, by upstream and result.",
	}, []string{"upstream", "result"})
	// ProxyFetches counts the file requests of proxy repos, by upstream and result: held (a hit),
	// fetched (a miss downloaded and ingested), shared (a miss another request fetched),
	// missing (the upstream does not have it), rejected (checksum, size, Validate), error.
	ProxyFetches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lajstrom_proxy_fetches_total", Help: "File requests of proxy repos, by upstream and result.",
	}, []string{"upstream", "result"})
	// Throttled counts the requests refused 429 by a throttle of internal/auth: account and
	// address (the failed logins of an account, of a client address), token (the requests to
	// /v2/token of a client address).
	Throttled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lajstrom_auth_throttled_total", Help: "Requests refused 429 by a throttle of internal/auth, by throttle.",
	}, []string{"throttle"})
	// buildInfo is 1 with the build in its labels.
	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lajstrom_build_info", Help: "Always 1; the labels say which build runs.",
	}, []string{"version", "commit", "goversion"})
)

func init() {
	registry.MustRegister(HTTPDuration, HTTPRequestSize, HTTPResponseSize, ProviderDuration, ProviderCircuitOpen, DBDuration, Ingest,
		ProxyUpstreamDuration, ProxyDocuments, ProxyFetches, Throttled, buildInfo,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

// SetBuild sets lajstrom_build_info to the build that runs.
func SetBuild(version, commit string) {
	buildInfo.Reset()
	buildInfo.WithLabelValues(version, commit, runtime.Version()).Set(1)
}

// Handler serves the metrics listener: GET /metrics, and /debug/pprof/ when withPprof. Every
// other path is 404.
func Handler(withPprof bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	if withPprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

// Status is the label of an HTTP status code.
func Status(code int) string { return strconv.Itoa(code) }
