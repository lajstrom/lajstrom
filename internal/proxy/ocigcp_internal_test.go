package proxy

import "testing"

func TestGCPRegistry(t *testing.T) {

	for u, want := range map[string]bool{
		"https://europe-west8-docker.pkg.dev": true,
		"https://gcr.io":                      true,
		"https://EU.GCR.IO.":                  true,
		"https://ghcr.io":                     false,
		"https://pkg.dev.example.com":         false,
		"http://127.0.0.1:5000":               false,
	} {
		if got := gcpRegistry(u); got != want {
			t.Errorf("gcpRegistry(%s) = %t, want %t", u, got, want)
		}
	}
}
