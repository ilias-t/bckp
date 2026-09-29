// Package eventlog is backpack's structured event log: JSONL files written
// through log/slog, size- and age-capped by a rotating writer, and read
// back (merged across files and sources) by `backpack logs`.
//
// Every line is one JSON object:
//
//	{"ts":"2026-09-28T21:24:03.512-04:00","level":"INFO","msg":"thermal.transition",
//	 "src":"engine","event":"thermal.transition","from":"NOMINAL","to":"FAIR","cpu_c":71}
//
// ts, level, msg and src are always present; event names the kind of
// record (see events.md for the taxonomy). The remaining fields depend on
// the event. Durations are strings ("1m30s"), temperatures are *_c numbers.
//
// The package is stdlib-only so the root daemon can use it.
package eventlog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Locations and names. This package is stdlib-only, so it cannot import
// internal/paths; SystemDir mirrors paths.DaemonLogDir, which callers
// outside the daemon should prefer.
const (
	// SystemDir holds the daemon's log: root:wheel 0755, files 0644 so the
	// user can read them without sudo (same as paths.DaemonLogDir).
	SystemDir = "/Library/Logs/backpack"
	// EngineName is the engine's log name (~/Library/Logs/backpack/engine.jsonl).
	EngineName = "engine"
	// DaemonName is the daemon's log name (/Library/Logs/backpack/backpackd.jsonl).
	DaemonName = "backpackd"
	// CLIName is the CLI's log name (~/Library/Logs/backpack/cli.jsonl),
	// separate from the engine's because a Writer rotates its file
	// in-process and two processes must not share one.
	CLIName = "cli"
)

// Sources (the src field).
const (
	SrcEngine = "engine"
	SrcDaemon = "daemon"
	SrcCLI    = "cli"
	SrcMacOS  = "macos"
)

// Options configure a log. Zero values mean the defaults.
type Options struct {
	// MaxFileBytes rotates the active file before it would exceed this
	// size (default 2 MB).
	MaxFileBytes int64
	// MaxTotalBytes caps the active plus rotated files (default 50 MB).
	MaxTotalBytes int64
	// MaxAge deletes rotated files last written longer ago (default 7d).
	MaxAge time.Duration
	// Src is the src field on every record (default: the log name).
	Src string
	// Level is the minimum level recorded (default Debug: record
	// everything; `backpack logs` filters on read).
	Level slog.Leveler
	// DirMode and FileMode default to 0700 and 0600 (per-user logs). The
	// daemon uses 0755 and 0644 so the user can read its log.
	DirMode, FileMode os.FileMode
	// Redact lists secrets (the ntfy topic) replaced by "<redacted>" in
	// every message and string or error field. Empty strings are ignored.
	Redact []string
	// Now is the clock used for rotation stamps and age purging (default
	// time.Now).
	Now func() time.Time
}

// WithConfig applies the user config keys log.max_mb and log.max_age
// (non-positive values keep the defaults).
func (o Options) WithConfig(maxMB int, maxAge time.Duration) Options {
	if maxMB > 0 {
		o.MaxTotalBytes = int64(maxMB) << 20
	}
	if maxAge > 0 {
		o.MaxAge = maxAge
	}
	return o
}

func (o Options) withDefaults() Options {
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if o.MaxFileBytes > o.MaxTotalBytes {
		o.MaxFileBytes = o.MaxTotalBytes
	}
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultMaxAge
	}
	if o.Level == nil {
		o.Level = slog.LevelDebug
	}
	if o.DirMode == 0 {
		o.DirMode = 0o700
	}
	if o.FileMode == 0 {
		o.FileMode = 0o600
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Open opens DIR/NAME.jsonl through a rotating Writer and returns a JSON
// slog logger with src set. Close the returned io.Closer on exit to flush
// the file.
func Open(dir, name string, o Options) (*slog.Logger, io.Closer, error) {
	o = o.withDefaults()
	w, err := NewWriter(dir, name, o)
	if err != nil {
		return nil, nil, err
	}
	src := o.Src
	if src == "" {
		src = name
	}
	return slog.New(NewHandler(w, o)).With("src", src), w, nil
}

// NewHandler returns the JSON handler Open uses, writing to w: "time" is
// renamed "ts" (RFC 3339, milliseconds, local offset) and secrets in
// o.Redact are masked. Tests use it with a buffer.
func NewHandler(w io.Writer, o Options) slog.Handler {
	o = o.withDefaults()
	var secrets []string
	for _, s := range o.Redact {
		if s != "" {
			secrets = append(secrets, s)
		}
	}
	redact := func(s string) string {
		for _, sec := range secrets {
			s = strings.ReplaceAll(s, sec, "<redacted>")
		}
		return s
	}
	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: o.Level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.String("ts", t.Format(TimeLayout))
				}
				return a
			}
			if len(secrets) == 0 {
				if err, ok := a.Value.Any().(error); ok && a.Value.Kind() == slog.KindAny {
					return slog.String(a.Key, err.Error())
				}
				return a
			}
			switch a.Value.Kind() {
			case slog.KindString:
				return slog.String(a.Key, redact(a.Value.String()))
			case slog.KindAny:
				switch v := a.Value.Any().(type) {
				case error:
					return slog.String(a.Key, redact(v.Error()))
				case []string:
					out := make([]string, len(v))
					for i, s := range v {
						out[i] = redact(s)
					}
					return slog.Any(a.Key, out)
				}
			}
			return a
		},
	})
}

// TimeLayout is the ts format: RFC 3339 with milliseconds.
const TimeLayout = "2006-01-02T15:04:05.000Z07:00"

// Log records one event at level: msg and the event field are both name,
// followed by args (key-value pairs, as for slog). A nil logger is a no-op.
func Log(l *slog.Logger, level slog.Level, name string, args ...any) {
	if l == nil {
		return
	}
	ctx := context.Background()
	if !l.Enabled(ctx, level) {
		return
	}
	l.Log(ctx, level, name, append([]any{"event", name}, args...)...)
}
