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
		{"bad size", []string{"registry", "probe", "--url", srv.URL, "--max-blob-size", "8MB"}, "--max-blob-size"},
		{"bad output", []string{"registry", "probe", "--url", srv.URL, "-o", "yaml"}, "--output"},
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
