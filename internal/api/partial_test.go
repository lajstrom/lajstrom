package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestDeleteVersionPartial: a DELETE of a version whose second variant fails answers the
// failure's status and names the variant deleted before it, which stays deleted (INV-13,
// #476): a 409 of the registry with its message, a 500 with its status text, both with the
// variant.
func TestDeleteVersionPartial(t *testing.T) {
	d := &scriptedDeleter{errs: map[string]error{}}
	f := newFixture(t, Options{Deleter: d})
	repo := f.create(t, "pypi", "libs")
	publish(t, f.st, repo.ID, "requests", "1.0", testNow, "sdist")
	publish(t, f.st, repo.ID, "requests", "1.0", testNow.Add(1), "py3-none-any")
	sdist, wheel := digest.FromString("requests1.0sdist"), digest.FromString("requests1.0py3-none-any")
	first := "deleted before the failure, and still deleted: 1.0 sdist (" + sdist.String() + ")"
	target := "/api/v1/repos/pypi/libs/packages/requests/versions/1.0"
	for _, tt := range []struct {
		err    error
		status int
		code   string
		want   []string
	}{
		{fmt.Errorf("ingest: %w", provider.ErrConflict), http.StatusConflict, codeConflict, []string{"refused to delete version 1.0", first}},
		{errors.New("index: the database is gone"), http.StatusInternalServerError, codeInternal, []string{"Internal Server Error; " + first}},
	} {
		d.errs[wheel.String()] = tt.err
		wantError(t, f.do(http.MethodDelete, target, "", admin), tt.status, tt.code, tt.want...)
	}
	wantLogs(t, f.logs, "the database is gone")
}
