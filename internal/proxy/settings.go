package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
)

// The bounds and defaults of a proxy repo's settings (docs/spec/proxy-and-group.md §2.2).
const (
	DefaultMetadataTTL = 10 * time.Minute
	DefaultNegativeTTL = 2 * time.Minute
	MaxMetadataTTL     = 168 * time.Hour
	MaxNegativeTTL     = 24 * time.Hour
	// MaxPatterns bounds each of allow and deny, MaxPatternLen each pattern, in bytes.
	MaxPatterns   = 256
	MaxPatternLen = 256
)

// Settings are a proxy repo's own settings, the column repos.upstream: the upstream it serves,
// named as the configuration names it, the TTLs of its metadata cache, offline mode and the
// lists of names it mirrors. The upstream's URL and credentials are never part of them.
type Settings struct {
	// Upstream is the name of one of upstreams[] of the configuration.
	Upstream string
	// MetadataTTL is how long a cached document is used without asking the upstream; 0
	// revalidates on every read.
	MetadataTTL time.Duration
	// NegativeTTL is how long a name the upstream does not have is remembered; 0 keeps none.
	NegativeTTL time.Duration
	// Offline means never ask the upstream: serve what is cached and held (§4.4).
	Offline bool
	// Allow and Deny are name patterns (§3): a name is fetched when Allow is empty or one of its
	// patterns matches, and none of Deny does.
	Allow []string
	Deny  []string
}

// SettingsError is a problem of a proxy repo's settings, written for the client: Field is the
// key it is about (upstream.metadataTTL, upstream.allow[3]). It wraps format.ErrInvalid, so the
// API answers 400 with the message as it is.
type SettingsError struct {
	Field   string
	Problem string
}

func (e *SettingsError) Error() string { return e.Field + ": " + e.Problem }

// Unwrap returns format.ErrInvalid.
func (e *SettingsError) Unwrap() error { return format.ErrInvalid }

func problem(field, msg string, args ...any) error {
	return &SettingsError{Field: field, Problem: fmt.Sprintf(msg, args...)}
}

// settingsJSON is the JSON form of Settings, as the API takes and shows it and the index keeps
// it: durations as Go duration strings ("10m").
type settingsJSON struct {
	Name        *string  `json:"name"`
	MetadataTTL *string  `json:"metadataTTL"`
	NegativeTTL *string  `json:"negativeTTL"`
	Offline     *bool    `json:"offline"`
	Allow       []string `json:"allow"`
	Deny        []string `json:"deny"`
}

// ParseSettings reads the JSON object of a proxy repo's settings, applies the defaults of the
// keys it leaves out and checks it whole: name present, each TTL a duration in its range, at
// most MaxPatterns patterns in each list, each pattern not empty, at most MaxPatternLen bytes,
// without a space or a control character. A key it does not know is refused. Errors are
// *SettingsError and name the field, upstream.<key>; whether the name is a configured upstream
// is the caller's to check.
func ParseSettings(raw json.RawMessage) (Settings, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return Settings{}, problem("upstream", "must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var j settingsJSON
	if err := dec.Decode(&j); err != nil {
		return Settings{}, decodeError(err)
	}
	if dec.Decode(new(any)) != io.EOF {
		return Settings{}, problem("upstream", "must be one JSON object, with nothing after it")
	}
	s := Settings{MetadataTTL: DefaultMetadataTTL, NegativeTTL: DefaultNegativeTTL, Allow: j.Allow, Deny: j.Deny}
	if j.Name == nil || *j.Name == "" {
		return Settings{}, problem("upstream.name", "must be one of upstreams[].name of the configuration")
	}
	s.Upstream = *j.Name
	if j.Offline != nil {
		s.Offline = *j.Offline
	}
	var err error
	if s.MetadataTTL, err = ttl("upstream.metadataTTL", j.MetadataTTL, DefaultMetadataTTL, MaxMetadataTTL); err != nil {
		return Settings{}, err
	}
	if s.NegativeTTL, err = ttl("upstream.negativeTTL", j.NegativeTTL, DefaultNegativeTTL, MaxNegativeTTL); err != nil {
		return Settings{}, err
	}
	if err := checkPatterns("upstream.allow", s.Allow); err != nil {
		return Settings{}, err
	}
	return s, checkPatterns("upstream.deny", s.Deny)
}

// decodeError turns an error of the JSON decoder into a *SettingsError.
func decodeError(err error) error {
	var typeErr *json.UnmarshalTypeError
	switch msg := err.Error(); {
	case errors.As(err, &typeErr) && typeErr.Field != "":
		return problem("upstream."+typeErr.Field, "must not be a JSON %s", typeErr.Value)
	case strings.HasPrefix(msg, "json: unknown field "):
		return problem("upstream", "%s is not a key: the keys are name, metadataTTL, negativeTTL, offline, allow and deny",
			strings.TrimPrefix(msg, "json: unknown field "))
	default:
		return problem("upstream", "must be a JSON object: %s", strings.TrimPrefix(msg, "json: "))
	}
}

// ttl reads a duration of the settings: def when absent, else a Go duration from 0 to most.
func ttl(field string, v *string, def, most time.Duration) (time.Duration, error) {
	if v == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*v)
	if err != nil || d < 0 || d > most {
		return 0, problem(field, "%q is not a duration from 0s to %s", *v, formatDuration(most))
	}
	return d, nil
}

// checkPatterns checks a list of name patterns (§2.5).
func checkPatterns(field string, patterns []string) error {
	if len(patterns) > MaxPatterns {
		return problem(field, "has %d patterns, at most %d are allowed", len(patterns), MaxPatterns)
	}
	for i, p := range patterns {
		key := fmt.Sprintf("%s[%d]", field, i)
		switch {
		case p == "":
			return problem(key, "must not be empty")
		case len(p) > MaxPatternLen:
			return problem(key, "is longer than %d bytes", MaxPatternLen)
		case strings.ContainsFunc(p, func(r rune) bool { return r <= ' ' || r == 0x7f }):
			return problem(key, "%q has a space or a control character", p)
		}
	}
	return nil
}

// JSON returns s as the API shows it and the index keeps it, with every key: the defaults are
// written out, the lists are [] when empty.
func (s Settings) JSON() json.RawMessage {
	allow, deny := s.Allow, s.Deny
	if allow == nil {
		allow = []string{}
	}
	if deny == nil {
		deny = []string{}
	}
	meta, neg := formatDuration(s.MetadataTTL), formatDuration(s.NegativeTTL)
	b, _ := json.Marshal(settingsJSON{Name: &s.Upstream, MetadataTTL: &meta, NegativeTTL: &neg, Offline: &s.Offline,
		Allow: allow, Deny: deny}) // strings and bools always marshal
	return b
}

// formatDuration writes d as a Go duration without the zero units at its end: 10m, 2h, 1h30m,
// 90s is 1m30s, zero is 0s.
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// Refuses reports whether the lists refuse the name key (§3), and why: the first pattern of
// Deny that matches, as "deny: <pattern>", or "allow" when Allow has patterns and none
// matches. Deny wins over Allow.
func (s Settings) Refuses(key string) (why string, refused bool) {
	for _, p := range s.Deny {
		if Match(p, key) {
			return "deny: " + p, true
		}
	}
	if len(s.Allow) == 0 {
		return "", false
	}
	for _, p := range s.Allow {
		if Match(p, key) {
			return "", false
		}
	}
	return "allow", true
}

// Match reports whether pattern matches the name key, ignoring ASCII case: * matches any run
// of characters, / and : included, and every other character matches itself (§3).
func Match(pattern, key string) bool {
	pattern, key = lowerASCII(pattern), lowerASCII(key)
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == key
	}
	rest, ok := strings.CutPrefix(key, parts[0])
	if !ok {
		return false
	}
	// Each part between two stars at its first place in what is left: a later place can only
	// leave less for the parts after it.
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, part)
		if i < 0 {
			return false
		}
		rest = rest[i+len(part):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

// lowerASCII maps the bytes A to Z to a to z and leaves every other byte as it is.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
