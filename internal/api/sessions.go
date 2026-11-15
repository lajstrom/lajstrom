package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// SessionList is the answer of GET /api/v1/repos/{format}/{repo}/sessions: a page of the repo's
// open publication sessions by id, and in Next the cursor of the page after it, absent on the
// last page.
type SessionList struct {
	Sessions []Session `json:"sessions"`
	Next     string    `json:"next,omitempty"`
}

// Session is an open publication session of a repo (docs/api.md, Open deploys): a maven deploy
// that waits for its maven-metadata.xml.
type Session struct {
	ID  int64  `json:"id"`
	Key string `json:"key"` // <groupId>:<artifactId>:<version>
	// Principal opened it, user:<uid> or token:<id>; PrincipalName is a user's name now.
	Principal     string `json:"principal"`
	PrincipalName string `json:"principalName,omitempty"`
	// Files are the files stored so far, in the order they first came.
	Files []SessionFile `json:"files"`
	// Refused are the files a checksum file disagreed with, by name, with the reason.
	Refused    map[string]string `json:"refused,omitempty"`
	Committing bool              `json:"committing"` // a commit of it runs
	CreatedAt  time.Time         `json:"createdAt"`
	UpdatedAt  time.Time         `json:"updatedAt"` // its last change
	ExpiresAt  time.Time         `json:"expiresAt"`
}

// SessionFile is a file of a session: its path below the repo, digest and size.
type SessionFile struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// sessionFiles is what the API reads of the files column of a publication session
// (docs/spec/index-schema.md, sessions).
type sessionFiles struct {
	Files      []SessionFile     `json:"files"`
	Refused    map[string]string `json:"refused"`
	Committing time.Time         `json:"committing"`
}

// listSessions answers a page of the repo's open publication sessions, by id: ?limit of them
// after the session of ?cursor (page). manage on the repo is decided before the repo is looked
// up (docs/spec/rbac.md §3): the files of a deploy are not published yet.
func (h *handler) listSessions(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	limit, after, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	var afterID int64
	if after != "" {
		if afterID, err = strconv.ParseInt(after, 10, 64); err != nil {
			return 0, nil, invalid("cursor: %q is not the next of a page of this list", r.URL.Query().Get("cursor"))
		}
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("list the sessions of", f, name, err)
	}
	rows, err := h.repos.Index.ListOpenSessions(ctx, repo.ID, h.repos.now(), afterID, limit+1)
	if err != nil {
		return 0, nil, fmt.Errorf("api: the sessions of %s/%s: %w", f, name, err)
	}
	out := SessionList{Sessions: make([]Session, 0, len(rows))}
	names := map[string]string{} // a page reads each user once
	for _, row := range rows[:min(limit, len(rows))] {
		s, err := h.session(r, row, names)
		if err != nil {
			return 0, nil, err
		}
		out.Sessions = append(out.Sessions, s)
	}
	if len(rows) > limit {
		out.Next = cursor(strconv.FormatInt(out.Sessions[len(out.Sessions)-1].ID, 10))
	}
	return http.StatusOK, out, nil
}

// session returns the API form of a sessions row, with the name of a user principal from names
// or read into it.
func (h *handler) session(r *http.Request, row index.Session, names map[string]string) (Session, error) {
	var st sessionFiles
	if len(row.Files) > 0 {
		if err := json.Unmarshal(row.Files, &st); err != nil {
			return Session{}, fmt.Errorf("api: the files of session %d: %w", row.ID, err)
		}
	}
	if st.Files == nil {
		st.Files = []SessionFile{}
	}
	s := Session{ID: row.ID, Key: row.Key, Principal: row.Principal, Files: st.Files, Committing: !st.Committing.IsZero(),
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(), ExpiresAt: row.ExpiresAt.UTC()}
	if len(st.Refused) > 0 {
		s.Refused = st.Refused
	}
	uid, ok := strings.CutPrefix(row.Principal, "user:")
	if !ok {
		return s, nil
	}
	name, known := names[uid]
	if !known {
		u, err := h.repos.Index.GetUserByUID(r.Context(), uid)
		switch {
		case err == nil && u.DeletedAt != nil:
			name = u.DeletedName
		case err == nil:
			name = u.Name
		case !errors.Is(err, index.ErrNotFound):
			return Session{}, fmt.Errorf("api: the principal of session %d: %w", row.ID, err)
		}
		names[uid] = name
	}
	s.PrincipalName = name
	return s, nil
}
