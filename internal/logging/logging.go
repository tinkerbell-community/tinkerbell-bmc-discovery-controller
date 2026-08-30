// Package logging builds the process-wide slog logger and derives named
// component loggers from it.
package logging

import (
	"fmt"
	"io"
	"log/slog"
)

// New returns a slog logger writing to w at the given level ("debug",
// "info", "warn", "error") in the given format ("json" or "text").
func New(level, format string, w io.Writer) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("unknown log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q: want json or text", format)
	}
}

// Component derives a named logger for one component; the name appears as
// the "logger" attribute on every record.
func Component(log *slog.Logger, name string) *slog.Logger {
	return log.With(slog.String("logger", name))
}
