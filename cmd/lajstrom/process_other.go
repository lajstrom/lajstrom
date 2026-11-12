//go:build !linux && !darwin

package main

// processScope is "" where the operating system tells nothing that Lajstrom reads: the process
// of a row is never known to be gone, and the row stays until it is stale.
func processScope() string { return "" }

// processStart is "": see processScope.
func processStart(int) string { return "" }

// processAlive is true: see processScope.
func processAlive(int) bool { return true }
