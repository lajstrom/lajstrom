package oidc

import (
	"html/template"
	"net/http"
	"sync"
	"time"
)

// page is what a page of the login says. Every field is escaped by html/template.
type page struct {
	Title   string
	Message string
	// Again links to a new login.
	Again bool
	// User, Token, Expires and Host are the token page's: the user, the token shown once, its
	// expiry and the registry's host for the examples.
	User, Token, Expires, Host string
	// Code and Device are the device page's: the code typed so far, and that it is that page.
	Code   string
	Device bool
}

// pageTemplate is every page of the login: no script, no external resource, no form but the
// device page's, a GET of /-/oidc/login.
var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Lajstrom: {{.Title}}</title>
<style>body{font-family:system-ui,sans-serif;max-width:42rem;margin:3rem auto;padding:0 1rem;line-height:1.5}
code,pre{background:#f2f2f2;padding:.1rem .3rem;border-radius:3px;word-break:break-all}pre{padding:.6rem;white-space:pre-wrap}
input{font-size:1.4rem;letter-spacing:.2rem;text-transform:uppercase;width:12rem}button{font-size:1.1rem}</style>
</head><body>
<h1>{{.Title}}</h1>
{{if .Message}}<p>{{.Message}}</p>{{end}}
{{if .Token}}<p>Logged in as <strong>{{.User}}</strong>. Your token, shown once, expires at {{.Expires}}:</p>
<pre>{{.Token}}</pre>
<p>Use it as the password of <code>{{.User}}</code> with every client, for example:</p>
<pre>docker login {{.Host}} -u {{.User}}
pip install --index-url https://{{.User}}:&lt;token&gt;@{{.Host}}/pypi/&lt;repo&gt;/simple/ &lt;package&gt;</pre>
<p>Copy it now: Lajstrom keeps only its hash. <code>lajstrom login</code> does this from a terminal.</p>{{end}}
{{if .Device}}<form method="get" action="/-/oidc/login">
<p>Enter the code that <code>lajstrom login</code> shows in your terminal. Continue only if you started it yourself, just now.</p>
<p><input name="user_code" value="{{.Code}}" autocomplete="off" maxlength="9" required> <button type="submit">Continue</button></p>
</form>{{end}}
{{if .Again}}<p><a href="/-/oidc/login">Log in again</a></p>{{end}}
</body></html>
`))

// writePage writes p as HTML with status and the headers of docs/spec/http-routing.md §7.
func writePage(w http.ResponseWriter, status int, p page) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, p) // the client went away
}

// writeDevicePage writes the device page with the code typed so far and a message.
func writeDevicePage(w http.ResponseWriter, status int, code, msg string) {
	writePage(w, status, page{Title: "Log in a terminal", Message: msg, Code: code, Device: true})
}

// limiter counts events per key in a fixed window: at most max per window. A key "" (a client
// address Lajstrom does not know) is never limited, as the throttles of failed logins
// (docs/auth.md). It keeps at most maxKeys keys; past them, a new key is not counted. Per
// replica, in memory, as those throttles (#282).
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	counts map[string]*windowCount
}

type windowCount struct {
	start time.Time
	n     int
}

const maxKeys = 10000

func newLimiter(maxEvents int, window time.Duration, now func() time.Time) *limiter {
	return &limiter{max: maxEvents, window: window, now: now, counts: map[string]*windowCount{}}
}

// wait returns how long key must wait before its next event counts, 0 when it need not.
func (l *limiter) wait(key string) time.Duration {
	if key == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.counts[key]
	now := l.now()
	if !ok || now.Sub(c.start) >= l.window {
		return 0
	}
	if c.n < l.max {
		return 0
	}
	return c.start.Add(l.window).Sub(now)
}

// add counts an event of key.
func (l *limiter) add(key string) {
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	c, ok := l.counts[key]
	switch {
	case ok && now.Sub(c.start) < l.window:
		c.n++
	case ok || len(l.counts) < maxKeys:
		l.counts[key] = &windowCount{start: now, n: 1}
	default:
		for k, c := range l.counts { // drop the windows that ended, then count when there is room
			if now.Sub(c.start) >= l.window {
				delete(l.counts, k)
			}
		}
		if len(l.counts) < maxKeys {
			l.counts[key] = &windowCount{start: now, n: 1}
		}
	}
}
