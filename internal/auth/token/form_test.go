package token

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReadFormTwice: the middleware and the facade both read the form of one request, and the
// second read gets the same form from Request.ParseForm, the body read once; scope repeats and
// the query names no field.
func TestReadFormTwice(t *testing.T) {
	body := "grant_type=password&username=alice&password=hunter2&scope=a+b&scope=c&service=example.com&client_id=containerd-client"
	r := httptest.NewRequest(http.MethodPost, "/v2/token?scope=q&username=query", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	for i := range 2 {
		f, fe := ReadForm(httptest.NewRecorder(), r)
		if fe != nil {
			t.Fatalf("read %d: %v", i, fe)
		}
		if f.Username != "alice" || f.Password != "hunter2" || fmt.Sprint(f.Scopes) != "[a b c]" || f.Service != "example.com" {
			t.Errorf("read %d: %s %s %v %s", i, f.Username, f.Password, f.Scopes, f.Service)
		}
	}
}

// TestFormSaysNoSecret: a form printed or logged says nothing of its user name or password.
func TestFormSaysNoSecret(t *testing.T) {
	f := &Form{Username: "typed-in-the-wrong-field", Password: "hunter2", Scopes: []string{"repository:raw/a/b:pull"}}
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("form", "form", f)
	for _, s := range []string{fmt.Sprint(f), fmt.Sprintf("%+v", f), fmt.Sprintf("%#v", f), logged.String()} {
		if strings.Contains(s, "hunter2") || strings.Contains(s, "typed-in-the-wrong-field") {
			t.Errorf("%q", s)
		}
	}
}
