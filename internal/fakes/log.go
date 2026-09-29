package fakes

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
)

// Record is one captured log record. Name is its "event" attribute (the
// engine's event name), else its message. Attrs are flattened: a group's
// members are keyed "group.key".
type Record struct {
	Level slog.Level
	Name  string
	Msg   string
	Attrs map[string]slog.Value
}

// Str returns attribute key as text, or "" when it is missing.
func (r Record) Str(key string) string {
	v, ok := r.Attrs[key]
	if !ok {
		return ""
	}
	return fmt.Sprint(v.Resolve().Any())
}

// Has reports whether attribute key is set.
func (r Record) Has(key string) bool {
	_, ok := r.Attrs[key]
	return ok
}

// Log captures structured log records at every level, so tests and
// `backpack simulate` can read the engine's event log back. Use Logger as
// engine.Deps.Logger.
type Log struct {
	sink   *logSink
	attrs  []slog.Attr
	prefix string
}

type logSink struct {
	mu       sync.Mutex
	recs     []Record
	onRecord func(Record)
}

var _ slog.Handler = (*Log)(nil)

// NewLog returns an empty capture. onRecord, when non-nil, is called with
// each record as it is logged, on the logging goroutine.
func NewLog(onRecord func(Record)) *Log {
	return &Log{sink: &logSink{onRecord: onRecord}}
}

// Logger returns a logger writing to l.
func (l *Log) Logger() *slog.Logger { return slog.New(l) }

// Records returns every record so far, in order.
func (l *Log) Records() []Record {
	l.sink.mu.Lock()
	defer l.sink.mu.Unlock()
	return slices.Clone(l.sink.recs)
}

// Named returns the records whose Name is name.
func (l *Log) Named(name string) []Record {
	var out []Record
	for _, r := range l.Records() {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

func (l *Log) Enabled(context.Context, slog.Level) bool { return true }

func (l *Log) Handle(_ context.Context, r slog.Record) error {
	rec := Record{Level: r.Level, Msg: r.Message, Attrs: map[string]slog.Value{}}
	for _, a := range l.attrs {
		addAttr(rec.Attrs, "", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		addAttr(rec.Attrs, l.prefix, a)
		return true
	})
	rec.Name = rec.Msg
	if v, ok := rec.Attrs["event"]; ok {
		rec.Name = v.String()
	}
	l.sink.mu.Lock()
	l.sink.recs = append(l.sink.recs, rec)
	l.sink.mu.Unlock()
	if l.sink.onRecord != nil {
		l.sink.onRecord(rec)
	}
	return nil
}

func addAttr(m map[string]slog.Value, prefix string, a slog.Attr) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range v.Group() {
			addAttr(m, p, g)
		}
		return
	}
	m[prefix+a.Key] = v
}

func (l *Log) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *l
	for _, a := range attrs {
		c.attrs = append(slices.Clone(c.attrs), slog.Attr{Key: l.prefix + a.Key, Value: a.Value})
	}
	return &c
}

func (l *Log) WithGroup(name string) slog.Handler {
	c := *l
	c.prefix += name + "."
	return &c
}
