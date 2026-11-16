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

// TestOriginContext: OriginFrom returns what WithOrigin stored, the zero Origin without it, and
// AddTo writes the keys of rbac.md §5 that are set, the reason of a change as changeReason.
func TestOriginContext(t *testing.T) {
	if o := OriginFrom(context.Background()); o != (Origin{}) {
		t.Errorf("OriginFrom(Background) = %+v, want the zero Origin", o)
	}
	o := Origin{Source: "cli", Host: "bastion", OSUser: "uid:1000", Actor: "alice@example.com", ChangeReason: "INC-42"}
	if got := OriginFrom(WithOrigin(context.Background(), o)); got != o {
		t.Errorf("OriginFrom = %+v, want %+v", got, o)
	}
	d := o.AddTo(map[string]any{"reason": "a rule's"})
	want := map[string]any{"reason": "a rule's", "source": "cli", "host": "bastion", "osUser": "uid:1000", "actor": "alice@example.com",
		"changeReason": "INC-42"}
	if len(d) != len(want) {
		t.Errorf("AddTo = %v, want %v", d, want)
	}
	for k, v := range want {
		if d[k] != v {
			t.Errorf("AddTo: %s = %v, want %v", k, d[k], v)
		}
	}
	if d := (Origin{Source: "api", RequestID: "r1", IP: "192.0.2.1"}).AddTo(map[string]any{}); len(d) != 2 {
		t.Errorf("AddTo of an API origin = %v, want source and requestId only", d)
	}
}
