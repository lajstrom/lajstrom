package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
)

// TestCheckPolicy: a policy is null or an object of the known keys, each checked; the result is
// compact with its keys sorted, and no key left is no policy.
func TestCheckPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, format, in, want, err string
	}{
		{"absent", "raw", "", "", ""},
		{"null", "raw", " null ", "", ""},
		{"empty object", "raw", `{}`, "", ""},
		{"null members only", "raw", `{"maxFileSize":null}`, "", ""},
		{"every key on maven", "maven", `{ "maven" : {"sessionTimeout":"45m"}, "blobRedirect":"never", "maxFileSize": 1048576 }`,
			`{"blobRedirect":"never","maven":{"sessionTimeout":"45m"},"maxFileSize":1048576}`, ""},
		{"a large size is kept exactly", "apt", `{"maxFileSize":9007199254740993}`, `{"maxFileSize":9007199254740993}`, ""},
		{"blobRedirect auto", "oci", `{"blobRedirect":"auto"}`, `{"blobRedirect":"auto"}`, ""},
		{"timeout bounds", "maven", `{"maven":{"sessionTimeout":"24h"}}`, `{"maven":{"sessionTimeout":"24h"}}`, ""},
		{"session files bounds", "maven", `{"maven":{"maxSessionFiles":999}}`, `{"maven":{"maxSessionFiles":999}}`, ""},
		{"session files and timeout", "maven", `{"maven":{"sessionTimeout":"1h","maxSessionFiles":1}}`, `{"maven":{"maxSessionFiles":1,"sessionTimeout":"1h"}}`, ""},
		{"array", "raw", `[]`, "", "policy: must be a JSON object or null"},
		{"string", "raw", `"x"`, "", "policy: must be a JSON object or null"},
		{"truncated", "raw", `{"a":`, "", "policy: must be a JSON object or null"},
		{"two values", "raw", `{} {}`, "", "policy: must be a JSON object or null"},
		{"unknown key", "raw", `{"maxFileSize":1,"maxFilesize":1}`, "", "policy.maxFilesize: unknown key; the keys are maxFileSize, blobRedirect, maven"},
		{"zero size", "raw", `{"maxFileSize":0}`, "", "policy.maxFileSize: must be an integer number of bytes, at least 1"},
		{"negative size", "raw", `{"maxFileSize":-1}`, "", "policy.maxFileSize: must be"},
		{"fractional size", "raw", `{"maxFileSize":1.5}`, "", "policy.maxFileSize: must be"},
		{"size as a string", "raw", `{"maxFileSize":"1M"}`, "", "policy.maxFileSize: must be"},
		{"size on oci", "oci", `{"maxFileSize":274877906944}`, `{"maxFileSize":274877906944}`, ""},
		{"blobRedirect other", "raw", `{"blobRedirect":"always"}`, "", `policy.blobRedirect: must be "auto" or "never"`},
		{"blobRedirect bool", "raw", `{"blobRedirect":false}`, "", `policy.blobRedirect: must be "auto" or "never"`},
		{"maven on raw", "raw", `{"maven":{"sessionTimeout":"45m"}}`, "", "policy.maven: only a maven repo has it, not a raw repo"},
		{"maven not an object", "maven", `{"maven":"45m"}`, "", "policy.maven: must be a JSON object"},
		{"maven unknown key", "maven", `{"maven":{"timeout":"45m"}}`, "", "policy.maven.timeout: unknown key; the keys are maxSessionFiles, sessionTimeout"},
		{"no session files", "maven", `{"maven":{"maxSessionFiles":0}}`, "", "policy.maven.maxSessionFiles: must be an integer from 1 to 999"},
		{"session files past a manifest", "maven", `{"maven":{"maxSessionFiles":1000}}`, "", "policy.maven.maxSessionFiles: must be"},
		{"session files a fraction", "maven", `{"maven":{"maxSessionFiles":2.5}}`, "", "policy.maven.maxSessionFiles: must be"},
		{"session files a string", "maven", `{"maven":{"maxSessionFiles":"256"}}`, "", "policy.maven.maxSessionFiles: must be"},
		{"timeout too short", "maven", `{"maven":{"sessionTimeout":"59s"}}`, "", "policy.maven.sessionTimeout: must be a duration from 1m to 24h"},
		{"timeout too long", "maven", `{"maven":{"sessionTimeout":"25h"}}`, "", "policy.maven.sessionTimeout: must be"},
		{"timeout a number", "maven", `{"maven":{"sessionTimeout":1800}}`, "", "policy.maven.sessionTimeout: must be"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkPolicy(tt.format, json.RawMessage(tt.in))
			wantPolicyResult(t, got, err, tt.want, tt.err)
		})
	}
}

// TestMergePolicy: PATCH merges as RFC 7396 says, and the merged policy is checked as a whole.
func TestMergePolicy(t *testing.T) {
	stored := `{"blobRedirect":"never","maven":{"sessionTimeout":"45m"},"maxFileSize":10}`
	for _, tt := range []struct {
		name, stored, patch, want, err string
	}{
		{"sets a key", stored, `{"maxFileSize":20}`, `{"blobRedirect":"never","maven":{"sessionTimeout":"45m"},"maxFileSize":20}`, ""},
		{"null removes a key", stored, `{"blobRedirect":null}`, `{"maven":{"sessionTimeout":"45m"},"maxFileSize":10}`, ""},
		{"nested objects merge", `{"maven":{"sessionTimeout":"45m"}}`, `{"maven":{"sessionTimeout":"2h"}}`, `{"maven":{"sessionTimeout":"2h"}}`, ""},
		{"an object left empty goes", stored, `{"maven":{"sessionTimeout":null}}`, `{"blobRedirect":"never","maxFileSize":10}`, ""},
		{"nothing left is no policy", `{"maxFileSize":10}`, `{"maxFileSize":null}`, "", ""},
		{"null clears", stored, `null`, "", ""},
		{"empty patch keeps", stored, `{}`, stored, ""},
		{"onto no policy", "", `{"blobRedirect":"never","maxFileSize":null}`, `{"blobRedirect":"never"}`, ""},
		{"the merge is checked", stored, `{"maxFileSize":0}`, "", "policy.maxFileSize: must be"},
		{"a bad stored key must be removed", `{"x":1,"maxFileSize":10}`, `{"maxFileSize":20}`, "", "policy.x: unknown key"},
		{"which a null member does", `{"x":1,"maxFileSize":10}`, `{"x":null}`, `{"maxFileSize":10}`, ""},
		{"a patch that is not an object", stored, `[1]`, "", "policy: must be a JSON object or null"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mergePolicy("maven", json.RawMessage(tt.stored), json.RawMessage(tt.patch))
			wantPolicyResult(t, got, err, tt.want, tt.err)
		})
	}
}

// wantPolicyResult checks a policy and the error that came with it: want when wantErr is empty,
// else an INVALID error whose message starts with wantErr.
func wantPolicyResult(t *testing.T, got json.RawMessage, err error, want, wantErr string) {
	t.Helper()
	switch {
	case wantErr == "" && err != nil:
		t.Fatalf("error %v, want %s", err, want)
	case wantErr == "" && string(got) != want:
		t.Errorf("policy %s, want %s", got, want)
	case wantErr != "" && (err == nil || !strings.HasPrefix(err.Error(), wantErr) || !errors.Is(err, format.ErrInvalid)):
		t.Errorf("policy %s, error %v; want an INVALID error starting %q", got, err, wantErr)
	}
}
