// Package probe measures a registry's row of the capability matrix (docs/registries/README.md):
// the method of the registry spikes as one run, for `lajstrom registry probe`.
//
// Run works in a scratch prefix below the registry's instance prefix, which it refuses when the
// registry already holds anything there. It pushes small manifests and blobs, asks the registry
// what each cell of the matrix asks, deletes what it pushed, and writes every cell in the
// README's vocabulary, with the requests and the trimmed answers that measured it. Cells that
// need time or a person (garbage collection, quotas, rate limits, the auth flavours) say that
// the probe does not measure them.
//
// What Lajstrom's provider makes of the registry, provider.Capabilities, is the generic
// provider's own probe (internal/provider/generic), which Run asks first; the row "What the
// probe finds" is its answer, and "Overrides needed" compares it with the measurements.
package probe
