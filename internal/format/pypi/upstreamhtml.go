package pypi

import (
	"html"
	"net/url"
	"path"
	"strings"
)

// htmlFiles reads the files of a project's page in the HTML form of PEP 503, as upstreamFile
// before listable: each <a> tag with an href, the file name the last segment of its path, the
// hash of its fragment (#sha256=…), data-requires-python, data-core-metadata or else
// data-dist-info-metadata (sha256=…), and data-yanked, its value the reason. What is in an HTML
// comment is not read.
func htmlFiles(page string) []upstreamFile {
	var out []upstreamFile
	for _, a := range anchors(page) {
		href, ok := a["href"]
		if !ok {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(href))
		if err != nil {
			continue
		}
		name, err := url.PathUnescape(path.Base(u.EscapedPath()))
		if err != nil {
			continue
		}
		f := upstreamFile{Filename: name, URL: u.String(), Hashes: map[string]string{}, RequiresPython: a["data-requires-python"]}
		if alg, sum, ok := strings.Cut(u.Fragment, "="); ok {
			f.Hashes[alg] = sum
		}
		for _, attr := range []string{"data-core-metadata", "data-dist-info-metadata"} {
			if alg, sum, ok := strings.Cut(a[attr], "="); ok && f.CoreMetadata == nil {
				f.CoreMetadata = mustJSON(map[string]string{alg: sum})
			}
		}
		if reason, yanked := a["data-yanked"]; yanked {
			f.Yanked = mustJSON(reason)
		}
		out = append(out, f)
	}
	return out
}

// anchors returns the attributes of the <a> tags of an HTML page, each by its name in lower case
// with its value unescaped, the first of a name; what is in an HTML comment is not read.
func anchors(page string) []map[string]string {
	var out []map[string]string
	for i := 0; i < len(page); {
		j := strings.IndexByte(page[i:], '<')
		if j < 0 {
			break
		}
		i += j
		rest := page[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				return out
			}
			i += 4 + end + 3
		case len(rest) > 2 && (rest[1] == 'a' || rest[1] == 'A') && (isSpace(rest[2]) || rest[2] == '>'):
			attrs, n := tagAttrs(rest[2:])
			out = append(out, attrs)
			i += 2 + n
		default:
			i++
		}
	}
	return out
}

// tagAttrs reads the attributes of a tag from s, which follows the tag's name, to the '>' that
// ends it, and returns how many bytes of s it read. A value is quoted with " or ', or unquoted to
// the next space or '>'.
func tagAttrs(s string) (map[string]string, int) {
	attrs := map[string]string{}
	i := 0
	for i < len(s) {
		i = skip(s, i, func(b byte) bool { return isSpace(b) || b == '/' })
		if i >= len(s) {
			break
		}
		if s[i] == '>' {
			return attrs, i + 1
		}
		start := i
		i = skip(s, i, func(b byte) bool { return !isSpace(b) && b != '=' && b != '>' && b != '/' })
		name := strings.ToLower(s[start:i])
		i = skip(s, i, isSpace)
		value := ""
		if i < len(s) && s[i] == '=' {
			value, i = attrValue(s, skip(s, i+1, isSpace))
		}
		if _, seen := attrs[name]; !seen && name != "" {
			attrs[name] = html.UnescapeString(value)
		}
	}
	return attrs, len(s)
}

// attrValue reads the value of an attribute that starts at s[i], quoted with " or ', or unquoted
// to the next space or '>', and returns it with the index after it.
func attrValue(s string, i int) (string, int) {
	if i < len(s) && (s[i] == '"' || s[i] == '\'') {
		end := strings.IndexByte(s[i+1:], s[i])
		if end < 0 {
			return s[i+1:], len(s)
		}
		return s[i+1 : i+1+end], i + 2 + end
	}
	end := skip(s, i, func(b byte) bool { return !isSpace(b) && b != '>' })
	return s[i:end], end
}

// skip returns the index of the first byte of s from i on that in does not take, len(s) when
// there is none.
func skip(s string, i int, in func(byte) bool) int {
	for i < len(s) && in(s[i]) {
		i++
	}
	return i
}

// isSpace reports whether b is ASCII white space as HTML reads it between attributes.
func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' }
