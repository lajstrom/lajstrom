package auth

import (
	"context"
	"testing"
)

func TestPrincipalContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want Principal
	}{
		{name: "absent", ctx: context.Background(), want: Anonymous},
		{name: "user", ctx: WithPrincipal(context.Background(), Principal{Kind: KindUser, Name: "alice"}), want: Principal{Kind: KindUser, Name: "alice"}},
		{name: "deploy token", ctx: WithPrincipal(context.Background(), Principal{Kind: KindToken, Name: "42"}), want: Principal{Kind: KindToken, Name: "42"}},
		{name: "anonymous", ctx: WithPrincipal(context.Background(), Anonymous), want: Anonymous},
		{name: "zero principal", ctx: WithPrincipal(context.Background(), Principal{}), want: Anonymous},
		{
			name: "innermost wins",
			ctx:  WithPrincipal(WithPrincipal(context.Background(), Principal{Kind: KindUser, Name: "alice"}), Principal{Kind: KindUser, Name: "bob"}),
			want: Principal{Kind: KindUser, Name: "bob"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PrincipalFrom(tt.ctx); got != tt.want {
				t.Errorf("PrincipalFrom = %#v, want %#v", got, tt.want)
			}
		})
	}
}
