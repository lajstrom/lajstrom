package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{in: "8MiB", want: 8 << 20},
		{in: "1GiB", want: 1 << 30},
		{in: "64KiB", want: 64 << 10},
		{in: "4194305", want: 4<<20 + 1},
		{in: "0", bad: true},
		{in: "-1MiB", bad: true},
		{in: "8MB", bad: true},
		{in: "lots", bad: true},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if (err != nil) != c.bad || (!c.bad && got != c.want) {
			t.Errorf("parseSize(%q) = %d, %v", c.in, got, err)
		}
	}
}

// takenRegistry answers the generic provider's probe and lists a repository below the scratch
// prefix lajstrom-probe-taken/, which makes the probe stop before it pushes anything; it
// remembers the Authorization headers it got.
func takenRegistry(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if _, _, ok := r.BasicAuth(); !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/":
			_, _ = io.WriteString(w, "{}")
		case "/v2/_catalog":
			_, _ = io.WriteString(w, `{"repositories":["lajstrom-probe-taken/create"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &auths
}

func TestRegistryProbe(t *testing.T) {
	srv, auths := takenRegistry(t)
	dir := t.TempDir()
	password := filepath.Join(dir, "password")
	if err := os.WriteFile(password, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "lajstrom.yaml")
	body := "server:\n  listen: \":8080\"\n  externalURL: https://art.example.com\ndatabase:\n  driver: sqlite\n  dsn: \"file:" + filepath.Join(dir, "lajstrom.db") + "\"\n" +
		"registries:\n  - name: taken\n    type: generic\n    url: " + srv.URL +
		"\n    auth:\n      username: probe\n      passwordEnv: PROBE_PASSWORD\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PROBE_PASSWORD": "s3cret"}
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"nothing named", []string{"registry", "probe"}, "--registry"},
		{"credentials with --registry", []string{"registry", "probe", "--config", cfg, "--registry", "taken", "--username", "x"}, "go with --url"},
		{"username without a password", []string{"registry", "probe", "--url", srv.URL, "--username", "x"}, "--password-file"},
		{"unknown registry", []string{"registry", "probe", "--config", cfg, "--registry", "nope"}, `no registry "nope"`},
		{"url with a path", []string{"registry", "probe", "--url", srv.URL + "/myorg"}, "--url must be the registry's base URL"},
		{"url with /v2", []string{"registry", "probe", "--url", srv.URL + "/v2/"}, "a project or an owner goes in --registry-prefix"},
		{"bad size", []string{"registry", "probe", "--url", srv.URL, "--max-blob-size", "8MB"}, "--max-blob-size"},
		{"bad output", []string{"registry", "probe", "--url", srv.URL, "-o", "yaml"}, "--output"},
		{"no routing heads", []string{"registry", "probe", "--url", srv.URL, "--routing-heads", "0"}, "--routing-heads"},
		{"no monotonic reads", []string{"registry", "probe", "--url", srv.URL, "--monotonic-reads", "0"}, "--monotonic-reads"},
		{"url, scratch taken", []string{"registry", "probe", "--url", srv.URL, "--username", "probe", "--password-file", password,
			"--scratch", "lajstrom-probe-taken"}, "scratch prefix is not empty"},
		{"configuration, scratch taken", []string{"registry", "probe", "--config", cfg, "--registry", "taken",
			"--scratch", "lajstrom-probe-taken"}, `registry "taken": probe: the scratch prefix is not empty`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := run(t, env, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want one with %q", err, c.wantErr)
			}
		})
	}
	basic := 0
	for _, a := range *auths {
		if a == "Basic cHJvYmU6czNjcmV0" { // probe:s3cret, from the file and from the environment
			basic++
		}
	}
	if basic == 0 {
		t.Errorf("the registry never got the credentials: %v", *auths)
	}
}

// TestProbeURLOfECR: --url of an ECR registry's host is a registry of type ecr, answered with
// the token the AWS SDK's credentials get; --username does not go with it (#19).
func TestProbeURLOfECR(t *testing.T) {
	const u = "https://123456789012.dkr.ecr.eu-west-1.amazonaws.com"
	r, err := probeFlags{prefix: "art"}.urlRegistry(u)
	if err != nil || r.Type != "ecr" || r.URL != u || r.Prefix != "art" || r.Auth.Username != "" {
		t.Errorf("urlRegistry(%s) = %+v, %v; want type ecr", u, r, err)
	}
	if _, err := (probeFlags{username: "AWS", passwordFile: "token"}).urlRegistry(u); err == nil || !strings.Contains(err.Error(), "AWS SDK") {
		t.Errorf("urlRegistry with --username = %v, want a refusal", err)
	}
	if r, err := (probeFlags{}).urlRegistry("http://localhost:5000"); err != nil || r.Type != "generic" {
		t.Errorf("urlRegistry(localhost) = %+v, %v; want type generic", r, err)
	}
}
