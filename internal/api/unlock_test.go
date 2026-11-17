package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestUnlockAPI: POST /api/v1/users/<user>/unlock ends a user's lock, 204, with a user.unlock row
// from the api when a lock was in force and none when not; a user that does not exist is 404.
// While auth.lockout.duration is set, a locked user is answered with lockedUntil, which moves no
// revision (docs/spec/http-routing.md §4).
func TestUnlockAPI(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{Lockout: 30 * time.Minute})
	f.addUsers(t, "admin", "alice")
	alice, err := f.st.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	before := f.do(http.MethodGet, "/api/v1/users/alice", "", admin).Header().Get("ETag")
	until := testNow.Add(time.Hour)
	if _, err := f.st.FailPassword(ctx, alice.ID, testNow, until, 1); err != nil {
		t.Fatal(err)
	}
	var got auth.User
	rr := f.do(http.MethodGet, "/api/v1/users/alice", "", admin)
	decodeJSON(t, rr, http.StatusOK, &got)
	if got.LockedUntil == nil || !got.LockedUntil.Equal(until) || rr.Header().Get("ETag") != before {
		t.Errorf("a locked user: lockedUntil %v, ETag %s; want %v, %s", got.LockedUntil, rr.Header().Get("ETag"), until, before)
	}
	var list UserList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/users", "", admin), http.StatusOK, &list)
	if len(list.Users) != 2 || list.Users[1].Name != "alice" || list.Users[1].LockedUntil == nil || list.Users[0].LockedUntil != nil {
		t.Errorf("the list: %+v, want alice locked and admin not", list.Users)
	}

	wantStatus(t, f.do(http.MethodPost, "/api/v1/users/alice/unlock", "", admin), http.StatusNoContent)
	if ev := oneAudit(t, f.st, auth.ActionUserUnlock); ev.Subject != "user:"+alice.UID || ev.details["source"] != SourceAPI ||
		ev.details["lockedUntil"] != until.Format(time.RFC3339) {
		t.Errorf("the user.unlock row: %+v", ev)
	}
	got = auth.User{}
	rr = f.do(http.MethodGet, "/api/v1/users/alice", "", admin)
	if decodeJSON(t, rr, http.StatusOK, &got); got.LockedUntil != nil {
		t.Errorf("after the unlock: lockedUntil %v", got.LockedUntil)
	}
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users/"+alice.UID+"/unlock", "", admin), http.StatusNoContent)
	if n := len(auditOf(t, f.st, auth.ActionUserUnlock)); n != 1 {
		t.Errorf("%d user.unlock rows after an unlock of a user not locked, want 1", n)
	}
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users/nobody/unlock", "", admin), http.StatusNotFound)
}
