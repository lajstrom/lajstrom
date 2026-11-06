package auth

import (
	"errors"
	"testing"
)

func TestPrincipalRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		p    Principal
		want string
	}{
		{name: "user", p: Principal{Kind: KindUser, Name: "alice"}, want: "user:alice"},
		{name: "deploy token", p: Principal{Kind: KindToken, Name: "42"}, want: "token:42"},
		{name: "group", p: Principal{Kind: KindGroup, Name: "ops"}, want: "group:ops"},
		{name: "anonymous", p: Anonymous, want: "anonymous"},
		{name: "colon in name", p: Principal{Kind: KindUser, Name: "a:b"}, want: "user:a:b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
			got, err := ParsePrincipal(tt.want)
			if err != nil {
				t.Fatalf("ParsePrincipal(%q): %v", tt.want, err)
			}
			if got != tt.p {
				t.Errorf("ParsePrincipal(%q) = %#v, want %#v", tt.want, got, tt.p)
			}
		})
	}
}

func TestParsePrincipalInvalid(t *testing.T) {
	tests := []string{
		"",
		":",
		"alice",
		"user:",
		"user",
		"admin:alice",
		"anonymous:x",
		"token:abc",
		"token:-1",
		"User:alice",
	}
	for _, s := range tests {
		t.Run(s, func(t *testing.T) {
			p, err := ParsePrincipal(s)
			if !errors.Is(err, ErrInvalidPrincipal) {
				t.Errorf("ParsePrincipal(%q) = %#v, %v; want ErrInvalidPrincipal", s, p, err)
			}
		})
	}
}
