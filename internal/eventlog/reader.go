package eventlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Event is one record read back from a log (or synthesized, e.g. from the
// macOS power log).
type Event struct {
	Time  time.Time
	Level slog.Level
	Src   string
	// Name is the event field; records without one use msg.
	Name string
	Msg  string
	// Attrs are the remaining fields in file order. Values are decoded
	// JSON: string, json.Number, bool, nil, map[string]any or []any.
	Attrs []Attr
	// Raw is the original line for events read from a file.
	Raw []byte
}

// Attr is one event field.
type Attr struct {
	Key   string
	Value any
}

// Get returns the value of the named field.
func (e Event) Get(key string) (any, bool) {
	for _, a := range e.Attrs {
		if a.Key == key {
			return a.Value, true
		}
	}
	return nil, false
}

// MarshalJSON returns the original line, or a JSONL-shaped object for
// synthesized events.
func (e Event) MarshalJSON() ([]byte, error) {
	if len(e.Raw) > 0 {
		return e.Raw, nil
	}
	var b bytes.Buffer
	b.WriteByte('{')
	field := func(k string, v any) error {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, err := json.Marshal(v)
		if err != nil {
			return err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
		return nil
	}
	_ = field("ts", e.Time.Format(TimeLayout))
	_ = field("level", e.Level.String())
	_ = field("msg", e.Msg)
	_ = field("src", e.Src)
	_ = field("event", e.Name)
	for _, a := range e.Attrs {
		if err := field(a.Key, a.Value); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// ParseLine decodes one JSONL record. Lines without a valid ts are
// rejected.
func ParseLine(line []byte) (Event, error) {
	line = bytes.TrimSpace(line)
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return Event{}, errors.New("eventlog: not a JSON object")
	}
	ev := Event{Raw: slices.Clone(line)}
	var haveTS bool
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return Event{}, err
		}
		key, _ := kt.(string)
		var v any
		if err := dec.Decode(&v); err != nil {
			return Event{}, err
		}
		s, isStr := v.(string)
		switch {
		case key == "ts" && isStr:
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return Event{}, fmt.Errorf("eventlog: bad ts: %w", err)
			}
			ev.Time, haveTS = t, true
		case key == "level" && isStr:
			var l slog.Level
			if l.UnmarshalText([]byte(s)) == nil {
				ev.Level = l
			}
		case key == "msg" && isStr:
			ev.Msg = s
		case key == "src" && isStr:
			ev.Src = s
		case key == "event" && isStr:
			ev.Name = s
		default:
			ev.Attrs = append(ev.Attrs, Attr{key, v})
		}
	}
	if _, err := dec.Token(); err != nil {
		return Event{}, err
	}
	if dec.More() {
		return Event{}, errors.New("eventlog: trailing data")
	}
	if !haveTS {
		return Event{}, errors.New("eventlog: no ts")
	}
	if ev.Name == "" {
		ev.Name = ev.Msg
	}
	return ev, nil
}

// Filter selects events. The zero value keeps Info and above from every
// source, at any time.
type Filter struct {
	// Since and Until bound the time window (zero = unbounded). Since is
	// inclusive, Until exclusive.
	Since, Until time.Time
	// MinLevel drops events below it. The zero value is Info.
	MinLevel slog.Level
	// Srcs keeps only these sources (empty keeps all).
	Srcs []string
}

// Match reports whether ev passes the filter.
func (f Filter) Match(ev Event) bool {
	if !f.Since.IsZero() && ev.Time.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !ev.Time.Before(f.Until) {
		return false
	}
	if ev.Level < f.MinLevel {
		return false
	}
	return len(f.Srcs) == 0 || slices.Contains(f.Srcs, ev.Src)
}

// WantsSrc reports whether events from src can pass the filter.
func (f Filter) WantsSrc(src string) bool {
	return len(f.Srcs) == 0 || slices.Contains(f.Srcs, src)
}

// Stats counts what a read saw.
type Stats struct {
	Files   int
	Lines   int
	Corrupt int
}

// LogFile is one event log file.
type LogFile struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// Files lists DIR/*.jsonl ordered by modification time, then name. A
// missing dir yields no files and no error.
func Files(dir string) ([]LogFile, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []LogFile
	for _, de := range ents {
		if !strings.HasSuffix(de.Name(), Ext) || !de.Type().IsRegular() {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, LogFile{filepath.Join(dir, de.Name()), fi.Size(), fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].ModTime.Before(out[j].ModTime)
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// FilesSince lists the files in dir that can hold events at or after
// since: every record in a file was written before its mtime.
func FilesSince(dir string, since time.Time) ([]LogFile, error) {
	files, err := Files(dir)
	var out []LogFile
	for _, f := range files {
		if since.IsZero() || !f.ModTime.Before(since) {
			out = append(out, f)
		}
	}
	return out, err
}

// ReadFile appends the events in path that match f. Corrupt lines (torn
// by a crash, or not JSON) are counted and skipped.
func ReadFile(path string, f Filter, st *Stats) ([]Event, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	return readEvents(fh, f, st)
}

func readEvents(r io.Reader, f Filter, st *Stats) ([]Event, error) {
	var out []Event
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			st.Lines++
			ev, perr := ParseLine(line)
			switch {
			case perr != nil:
				st.Corrupt++
			case f.Match(ev):
				out = append(out, ev)
			}
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// ReadDirs reads every *.jsonl file in dirs that may hold events in the
// filter's window and returns the matching events in time order (ties
// keep file order). A missing dir is skipped; other per-dir or per-file
// errors are joined and returned alongside the events that could be read.
func ReadDirs(dirs []string, f Filter) ([]Event, Stats, error) {
	var (
		out  []Event
		st   Stats
		errs []error
	)
	for _, dir := range dirs {
		files, err := FilesSince(dir, f.Since)
		if err != nil {
			errs = append(errs, err)
		}
		for _, lf := range files {
			st.Files++
			evs, err := ReadFile(lf.Path, f, &st)
			if err != nil {
				errs = append(errs, err)
			}
			out = append(out, evs...)
		}
	}
	SortEvents(out)
	return out, st, errors.Join(errs...)
}

// SortEvents sorts by time, keeping the existing order of equal times.
func SortEvents(evs []Event) {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Time.Before(evs[j].Time) })
}
