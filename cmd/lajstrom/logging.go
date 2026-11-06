package main

import (
	"fmt"
	"io"
	"log/slog"
)

// newLogger returns the process logger: level is debug, info, warn or error, format is text
// or json.
func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "info":
		l = slog.LevelInfo
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		return nil, fmt.Errorf("log level %q is not debug, info, warn or error", level)
	}
	opts := &slog.HandlerOptions{Level: l}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("log format %q is not text or json", format)
	}
}
