package main

import (
	"strings"
	"testing"
)

// TestAdminWebhook: admin webhook creates a webhook of a mask and prints its secret once, lists,
// changes, rotates and deletes it, refuses a URL the egress rules refuse, and needs the master
// key to create one.
func TestAdminWebhook(t *testing.T) {
	cfg, _ := writeKeyConfig(t, t.TempDir())
	env := map[string]string{"LAJSTROM_MASTER_KEY": testMasterKey}
	runEnv(t, cfg, env, []step{{args: []string{"migrate"}}})
	outs := runEnv(t, cfg, env, []step{
		{args: []string{"admin", "webhook", "create", "--mask", "raw/*", "--url", "https://ci.example.com/hooks/tok",
			"--events", "version.published"}, wantOut: `"secret":"whsec_`},
		{args: []string{"admin", "webhook", "list"}, wantOut: "raw/*"},
		{args: []string{"admin", "webhook", "list"}},
		{args: []string{"admin", "webhook", "update", "1", "--active=false"}, wantOut: `"active":false`},
		{args: []string{"admin", "webhook", "rotate", "1"}, wantOut: `{"secret":"whsec_`},
		{args: []string{"admin", "webhook", "create", "--mask", "raw/*", "--url", "http://ci.example.com/"},
			wantErr: "url: must be https"},
		{args: []string{"admin", "webhook", "create", "--mask", "nope/*", "--url", "https://ci.example.com/"},
			wantErr: `mask: "nope" is not a format of this build`},
		{args: []string{"admin", "webhook", "deliveries", "1"}, wantOut: "EVENT"},
		{args: []string{"admin", "webhook", "delete", "1"}},
		{args: []string{"admin", "webhook", "delete", "1"}, wantErr: "webhook 1 does not exist"},
	})
	if strings.Contains(outs[2], "whsec_") {
		t.Errorf("list shows a secret: %s", outs[2])
	}
	runEnv(t, cfg, nil, []step{{args: []string{"admin", "webhook", "create", "--mask", "raw/*", "--url", "https://ci.example.com/"},
		wantErr: "webhooks need the master key"}})
}
