package generic

import (
	"errors"
	"io"
	"net/url"
)

// RedirectError is the failure of a hop of a blob's redirect chain that left the registry's
// origin, or of the body it answered: a CDN or object storage that the registry sent the blob
// to. Host names the hop, which an operator behind an egress filter must allow, and which the
// error of the registry's own host would not name (#889). Its message is the error's; errors.Is
// and errors.As see through it.
type RedirectError struct {
	Host string
	Err  error
}

func (e *RedirectError) Error() string { return e.Err.Error() }

// Unwrap returns the hop's error.
func (e *RedirectError) Unwrap() error { return e.Err }

// viaRedirect returns err, the failure of a request of a blob's redirect chain to u, as a
// *RedirectError when u is off the registry's origin; nil stays nil.
func (p *Provider) viaRedirect(u *url.URL, err error) error {
	if err == nil || p.sameOrigin(u) {
		return err
	}
	return &RedirectError{Host: u.Host, Err: err}
}

// redirectBody is the body that a hop off the registry's origin answered: a read that fails
// does so with a *RedirectError that names the hop.
type redirectBody struct {
	io.ReadCloser
	host string
}

func (b *redirectBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = &RedirectError{Host: b.host, Err: err}
	}
	return n, err
}
