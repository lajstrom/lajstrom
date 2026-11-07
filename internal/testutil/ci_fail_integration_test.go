//go:build integration

package testutil

import "testing"

// TestDeliberateFailure fails on purpose, to see the integration job of #16 go red. The commit
// that adds it is reverted by the next one.
func TestDeliberateFailure(t *testing.T) {
	t.Fatal("deliberate failure: verifying the integration job of #16")
}
