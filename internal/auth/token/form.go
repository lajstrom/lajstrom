package token

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
)

// The token request in the OAuth2 form (docs/spec/http-routing.md §2, #908): POST /v2/token with
// the password grant of RFC 6749 §4.3, which containerd posts before it tries GET whenever it
// has a secret. The server's authentication middleware reads the form and checks its user name
// and password as it checks Basic credentials; the facade takes its scopes and its service.

// MaxFormSize is the largest body of a token request in the OAuth2 form: 8 KiB, the bound of
// /-/oidc/token. The form holds a grant type, a user name, a password of at most 1 KiB or a
// token, a service and the scopes; containerd asks for one or two.
const MaxFormSize = 8 << 10

// GrantPassword is the one grant type of the form: the user name and the secret of HTTP Basic
// credentials, as the resource owner's password credentials of RFC 6749 §4.3.
const GrantPassword = "password"

// The OAuth2 error codes of a form that is refused (RFC 6749 §5.2).
const (
	ErrorInvalidRequest       = "invalid_request"
	ErrorUnsupportedGrantType = "unsupported_grant_type"
)

// Form is a token request in the OAuth2 form. Its String, GoString and LogValue say nothing of
// the user name or the password: a user name may be a password typed in the wrong field.
type Form struct {
	Username, Password string
	// Scopes are the scope parameters, as GET's (ParseScopes): each may hold several scopes
	// separated by spaces, as containerd sends them.
	Scopes []string
	// Service is the service parameter, empty without one.
	Service string
}

// String describes f without its credentials.
func (f *Form) String() string { return fmt.Sprintf("token form: %d scope parameters", len(f.Scopes)) }

// GoString returns the same for %#v.
func (f *Form) GoString() string { return f.String() }

// LogValue logs the number of scope parameters only.
func (f *Form) LogValue() slog.Value { return slog.IntValue(len(f.Scopes)) }

// FormError is a form that ReadForm refuses: the status of the answer, 400 or 413, and its
// OAuth2 error code and description (WriteFormError). A description names no value of the form.
type FormError struct {
	Status      int
	Code        string
	Description string
}

func (e *FormError) Error() string { return "token: form refused: " + e.Description }

// ReadForm reads the OAuth2 form of r, a POST of the token endpoint, and returns it, or the
// FormError that refuses it:
//
//   - 413 invalid_request for a body over MaxFormSize, before it is read when its
//     Content-Length says so (http.MaxBytesReader otherwise);
//   - 400 invalid_request for a media type other than application/x-www-form-urlencoded
//     (parameters such as charset allowed), a request that does not parse as a form, a form
//     without grant_type, username or password, or with one of them or service more than once
//     (RFC 6749 §3.1);
//   - 400 unsupported_grant_type for a grant_type other than password, refresh_token included:
//     Lajstrom issues no refresh token.
//
// scope may repeat; client_id and every other field are ignored. The fields are read from the
// body only, never from the query. ReadForm may be called twice on one request, by the
// middleware and by the facade: Request.ParseForm reads the body once.
func ReadForm(w http.ResponseWriter, r *http.Request) (*Form, *FormError) {
	if r.ContentLength > MaxFormSize {
		return nil, tooLarge()
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		return nil, invalid("the body is not application/x-www-form-urlencoded")
	}
	if r.PostForm == nil && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, MaxFormSize)
	}
	if err := r.ParseForm(); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, tooLarge()
		}
		return nil, invalid("the request does not parse as a form")
	}
	v := r.PostForm
	for _, k := range []string{"grant_type", "username", "password", "service"} {
		if len(v[k]) > 1 {
			return nil, invalid("the form has " + k + " more than once")
		}
	}
	if !v.Has("grant_type") {
		return nil, invalid("the form has no grant_type")
	}
	if v.Get("grant_type") != GrantPassword {
		return nil, &FormError{Status: http.StatusBadRequest, Code: ErrorUnsupportedGrantType,
			Description: "the one grant type is " + GrantPassword + ": Lajstrom issues no refresh token"}
	}
	for _, k := range []string{"username", "password"} {
		if !v.Has(k) {
			return nil, invalid("the form has no " + k)
		}
	}
	return &Form{Username: v.Get("username"), Password: v.Get("password"), Scopes: v["scope"], Service: v.Get("service")}, nil
}

// invalid is the FormError invalid_request with the description d.
func invalid(d string) *FormError {
	return &FormError{Status: http.StatusBadRequest, Code: ErrorInvalidRequest, Description: d}
}

// tooLarge is the FormError of a body over MaxFormSize.
func tooLarge() *FormError {
	return &FormError{Status: http.StatusRequestEntityTooLarge, Code: ErrorInvalidRequest,
		Description: fmt.Sprintf("the form is larger than %d bytes", MaxFormSize)}
}

// WriteFormError answers e as RFC 6749 §5.2 says: its status with
// {"error": <code>, "error_description": <description>} and Cache-Control: no-store.
func WriteFormError(w http.ResponseWriter, e *FormError) {
	body, _ := json.Marshal(map[string]string{"error": e.Code, "error_description": e.Description}) // strings always encode
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(e.Status)
	_, _ = w.Write(append(body, '\n'))
}
