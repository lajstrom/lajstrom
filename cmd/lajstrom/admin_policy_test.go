package main

import "testing"

// TestAdminPolicy: admin policy rule creates a rule and prints it, lists, refuses a range on a
// format without an order, and deletes it.
func TestAdminPolicy(t *testing.T) {
	cfg, _ := writeKeyConfig(t, t.TempDir())
	runEnv(t, cfg, nil, []step{{args: []string{"migrate"}}})
	runEnv(t, cfg, nil, []step{
		{args: []string{"admin", "policy", "rule", "create", "--mask", "raw/*", "--name", "evil", "--version", "==1.0",
			"--reason", "CVE-2026-1234", "--expires", "72h"}, wantOut: `"repoMask":"raw/*"`},
		{args: []string{"admin", "policy", "rule", "list"}, wantOut: "CVE-2026-1234"},
		{args: []string{"admin", "policy", "rule", "create", "--mask", "raw/x", "--name", "a", "--version", ">=1",
			"--reason", "r"}, wantErr: "version: a range needs"},
		{args: []string{"admin", "policy", "rule", "delete", "1"}},
		{args: []string{"admin", "policy", "rule", "delete", "1"}, wantErr: "not found"},
	})
}
