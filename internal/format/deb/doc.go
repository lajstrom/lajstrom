// Package deb is the apt format plugin (plugin name "apt", artifact type deb): .deb control
// parsing, Packages and Release rendering, signing through internal/keys, and pool routes.
// See docs/spec/http-routing.md §3 (apt) and docs/spec/manifest-profile.md.
//
// Filled in by issues #39 (parse), #40 (index rendering) and #42 (pool and upload).
package deb
