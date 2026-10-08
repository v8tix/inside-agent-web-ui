// Package logging builds the application logger.
package logging

import (
	"io"
	"log/slog"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
)

// New returns a logger writing JSON or text records to w at the configured
// level. Config validation guarantees the level and format are known values.
func New(cfg config.App, w io.Writer) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler = slog.NewJSONHandler(w, opts)
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler).With("app", "agent-web-ui")
}
