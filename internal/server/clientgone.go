package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
)

// StatusClientClosedRequest is the status of the access line and of the HTTP metrics of a
// request whose client went away before it was answered (nginx's 499). No client receives it.
const StatusClientClosedRequest = 499

// errAborted is the cause with which Run cancels the context of every request still running at
// the shutdown deadline: the abort is Lajstrom's, not the client's (ClientGone).
var errAborted = errors.New("server: request aborted at the shutdown deadline")

// ClientGone reports whether the client of r went away: r's context was cancelled with no cause
// of its own, which is how net/http cancels it when it sees the connection close or the stream
// reset. A deadline, Lajstrom's own, is not the client's leaving, and neither is the abort at the
// shutdown deadline, whose cause is errAborted.
func ClientGone(r *http.Request) bool {
	// ==, not errors.Is: a cause that wraps context.Canceled is somebody's own, not net/http's.
	return context.Cause(r.Context()) == context.Canceled //nolint:errorlint // see above
}

// FailLevel is the level of the line that logs why a request failed: Error, Debug when the
// client went away, since then the request failed because its context was cancelled and nobody
// reads the answer.
func FailLevel(r *http.Request) slog.Level {
	if ClientGone(r) {
		return slog.LevelDebug
	}
	return slog.LevelError
}
