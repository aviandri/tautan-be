// Package logx configures structured logging with log/slog.
package logx

import (
	"io"
	"log/slog"
	"os"
)

// Setup installs a slog default logger. format is "json" or "text"; level is
// debug|info|warn|error. slog.SetDefault also routes the standard library's
// log.Printf through the handler (at INFO), so older call sites come out as
// structured records too.
func Setup(format, level string) *slog.Logger {
	return setup(os.Stderr, format, level)
}

func setup(w io.Writer, format, level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}
