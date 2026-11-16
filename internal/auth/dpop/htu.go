package dpop

import (
	"net"
	"net/url"
	"strings"
)

// Normalize returns the http(s) URI raw without its query and fragment, normalized as RFC 9449
// §4.3 asks before htu is compared: syntax-based (RFC 3986 §6.2.2: the scheme and the host in
// lower case, a percent-encoded unreserved character decoded and the hex digits of the others in
// upper case, the dot segments removed) and scheme-based (§6.2.3: no default port, an empty path
// as "/"). It reports false for anything that is not an absolute http or https URI with a host,
// or that has user information.
func Normalize(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", false
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	path := removeDotSegments(normalizeEscapes(u.EscapedPath()))
	if path == "" {
		path = "/"
	}
	return scheme + "://" + host + path, true
}

// normalizeEscapes decodes the percent-encoded unreserved characters of the path p and writes
// the hex digits of every other percent-encoding in upper case (RFC 3986 §6.2.2.1, §6.2.2.2).
func normalizeEscapes(p string) string {
	if !strings.Contains(p, "%") {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '%' && i+2 < len(p) && isHex(p[i+1]) && isHex(p[i+2]) {
			if c := unhex(p[i+1])<<4 | unhex(p[i+2]); unreserved(c) {
				b.WriteByte(c)
			} else {
				b.WriteString("%" + strings.ToUpper(p[i+1:i+3]))
			}
			i += 2
			continue
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// removeDotSegments is the algorithm of RFC 3986 §5.2.4.
func removeDotSegments(in string) string {
	var out []string // each segment with its leading "/", if any
	pop := func() {
		if len(out) > 0 {
			out = out[:len(out)-1]
		}
	}
	for in != "" {
		switch {
		case strings.HasPrefix(in, "../"):
			in = in[3:]
		case strings.HasPrefix(in, "./"):
			in = in[2:]
		case strings.HasPrefix(in, "/./"):
			in = in[2:]
		case in == "/.":
			in = "/"
		case strings.HasPrefix(in, "/../"):
			in = in[3:]
			pop()
		case in == "/..":
			in = "/"
			pop()
		case in == "." || in == "..":
			in = ""
		default:
			end := strings.IndexByte(in[1:], '/') + 1
			if end == 0 {
				end = len(in)
			}
			out = append(out, in[:end])
			in = in[end:]
		}
	}
	return strings.Join(out, "")
}

// unreserved reports whether c is an unreserved character of RFC 3986 §2.3.
func unreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}
