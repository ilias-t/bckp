package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/eventlog"
	"github.com/ilias-t/bckp/internal/paths"
)

// `backpack logs`: the engine's and the daemon's event logs merged with the
// macOS power log (Sleep/Wake/DarkWake from `pmset -g log`) into one
// timeline. See internal/eventlog/events.md for the event names.

const (
	logsDefaultSince = 3 * time.Hour
	// logsFollowInterval is the --follow poll interval for the event logs.
	logsFollowInterval = time.Second
	// logsPmsetInterval is how often --follow re-reads `pmset -g log`
	// (it takes a few seconds and ~20 MB).
	logsPmsetInterval = time.Minute
	// bundleEngineLogTail is how much of each unbounded stdio log
	// (engine.log, backpackd.log: panics, early startup errors) goes into
	// a bundle.
	bundleEngineLogTail = 1 << 20
)

var pmsetLogArgs = []string{"-g", "log"}

type logsOpts struct {
	since, level, src string
	follow, asJSON    bool
	bundle            bool
	bundlePath        string
}

func cmdLogs(e *env, args []string) int {
	var o logsOpts
	args, o.bundle, o.bundlePath = extractOptionalFlag(args, "bundle")
	fset := newFlags(e, "logs")
	fset.StringVar(&o.since, "since", "", "`WHEN` to start: a duration back (90m, 2h, 1d), a clock time today (15:04) or RFC 3339 (default 3h ago)")
	fset.BoolVar(&o.follow, "follow", false, "keep printing new events")
	fset.BoolVar(&o.follow, "f", false, "keep printing new events")
	fset.BoolVar(&o.asJSON, "json", false, "print JSONL")
	fset.StringVar(&o.level, "level", "info", "minimum `LEVEL`: debug (adds the per-minute samples), info, warn or error")
	fset.StringVar(&o.src, "src", "", "`SOURCES` to show, comma-separated: engine, daemon, macos (default all)")
	fset.extra = [][2]string{{"    --bundle [PATH]", "write a .tar.gz of the logs (config and ntfy topic redacted) for a bug report"}}
	pos, code, ok := e.parseArgs(fset, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		e.errorf("logs takes no arguments (did you mean --since %s?)", pos[0])
		return exitUsage
	}
	now := e.now()
	since, err := parseSince(o.since, now)
	if err != nil {
		e.errorf("--since: %v", err)
		return exitUsage
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(o.level)); err != nil {
		e.errorf("--level must be debug, info, warn or error (got %q)", o.level)
		return exitUsage
	}
	filter := eventlog.Filter{Since: since, MinLevel: lvl}
	if o.src != "" {
		for _, s := range strings.Split(o.src, ",") {
			s = strings.TrimSpace(s)
			switch s {
			case eventlog.SrcEngine, eventlog.SrcDaemon, eventlog.SrcMacOS, eventlog.SrcCLI:
				filter.Srcs = append(filter.Srcs, s)
			default:
				e.errorf("--src: unknown source %q (want engine, daemon or macos)", s)
				return exitUsage
			}
		}
	}
	if o.bundle {
		if o.follow {
			e.errorf("--bundle and --follow do not mix")
			return exitUsage
		}
		return e.writeLogBundle(o.bundlePath, filter, now)
	}

	ctx, cancel := e.signalContext(context.Background())
	defer cancel()
	evs, notes := e.collectEvents(ctx, filter)
	for _, n := range notes {
		e.errorf("%s", n)
	}
	p := &timelinePrinter{w: e.stdout, json: o.asJSON}
	for _, ev := range evs {
		p.print(ev)
	}
	if len(evs) == 0 && !o.asJSON {
		e.errorf("no events since %s (logs: %s, %s)", since.Format("Jan 2 15:04"), e.user.LogDir, e.sys(paths.DaemonLogDir))
	}
	if !o.follow {
		return exitOK
	}
	return e.followLogs(ctx, filter, p, evs)
}

// extractOptionalFlag removes --name, --name=VALUE or --name VALUE (when
// VALUE does not start with '-') from args: the flag package cannot express
// a flag whose value is optional.
func extractOptionalFlag(args []string, name string) (rest []string, set bool, value string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(rest, args[i:]...), set, value
		}
		bare := strings.TrimLeft(a, "-")
		if !strings.HasPrefix(a, "-") || (bare != name && !strings.HasPrefix(bare, name+"=")) {
			rest = append(rest, a)
			continue
		}
		set = true
		if v, ok := strings.CutPrefix(bare, name+"="); ok {
			value = v
		} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			value = args[i+1]
			i++
		}
	}
	return rest, set, value
}

var dayDurRE = regexp.MustCompile(`^(\d+)d(.*)$`)

// parseSince resolves --since: "" (3h), a Go duration or Nd[duration]
// back from now, "15:04" today (yesterday if that is in the future), a
// local "2006-01-02 15:04", or RFC 3339.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return now.Add(-logsDefaultSince), nil
	}
	if m := dayDurRE.FindStringSubmatch(s); m != nil {
		days, _ := strconv.Atoi(m[1])
		d := time.Duration(days) * 24 * time.Hour
		if m[2] != "" {
			rest, err := time.ParseDuration(m[2])
			if err != nil {
				return time.Time{}, fmt.Errorf("bad duration %q", s)
			}
			d += rest
		}
		return now.Add(-d), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			d = -d
		}
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, now.Location()); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("15:04", s, now.Location()); err == nil {
		y, mo, d := now.Date()
		at := time.Date(y, mo, d, t.Hour(), t.Minute(), 0, 0, now.Location())
		if at.After(now) {
			at = at.AddDate(0, 0, -1)
		}
		return at, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a duration (2h, 1d), a clock time (15:04) or a timestamp (RFC 3339)", s)
}

// logDirs are the engine's (per-user) and the daemon's (system) log dirs.
func (e *env) logDirs(f eventlog.Filter) []string {
	var dirs []string
	if f.WantsSrc(eventlog.SrcEngine) || f.WantsSrc(eventlog.SrcCLI) {
		dirs = append(dirs, e.user.LogDir)
	}
	if f.WantsSrc(eventlog.SrcDaemon) {
		dirs = append(dirs, e.sys(paths.DaemonLogDir))
	}
	return dirs
}

// collectEvents reads the event logs and the macOS power log for the
// filter's window. Problems that leave the timeline incomplete are
// returned as notes for stderr.
func (e *env) collectEvents(ctx context.Context, f eventlog.Filter) ([]eventlog.Event, []string) {
	var notes []string
	evs, st, err := eventlog.ReadDirs(e.logDirs(f), f)
	if err != nil {
		notes = append(notes, fmt.Sprintf("some log files could not be read: %v", err))
	}
	if st.Corrupt > 0 {
		notes = append(notes, fmt.Sprintf("skipped %d unreadable log line(s) (e.g. torn by a crash)", st.Corrupt))
	}
	if f.WantsSrc(eventlog.SrcMacOS) {
		mac, err := e.pmsetEvents(ctx, f)
		if err != nil {
			notes = append(notes, fmt.Sprintf("macOS power log unavailable: %v", err))
		}
		evs = append(evs, mac...)
		eventlog.SortEvents(evs)
	}
	return evs, notes
}

// pmsetEvents runs `pmset -g log` (read-only) and returns its sleep/wake
// events that pass f.
func (e *env) pmsetEvents(ctx context.Context, f eventlog.Filter) ([]eventlog.Event, error) {
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := e.exec.Run(rctx, paths.BinPmset, pmsetLogArgs...)
	if err != nil {
		return nil, err
	}
	all, err := eventlog.ParsePmsetLog(bytes.NewReader(res.Stdout), f.Since, f.Until)
	var out []eventlog.Event
	for _, ev := range all {
		if f.Match(ev) {
			out = append(out, ev)
		}
	}
	return out, err
}

// timelinePrinter prints events as JSONL or as the human timeline, with a
// date line whenever the day changes.
type timelinePrinter struct {
	w       io.Writer
	json    bool
	lastDay string
}

func (p *timelinePrinter) print(ev eventlog.Event) {
	if p.json {
		b, err := json.Marshal(ev)
		if err == nil {
			fmt.Fprintf(p.w, "%s\n", b)
		}
		return
	}
	t := ev.Time.Local()
	if day := t.Format("2006-01-02"); day != p.lastDay {
		fmt.Fprintf(p.w, "--- %s ---\n", t.Format("Mon 2006-01-02"))
		p.lastDay = day
	}
	fmt.Fprintln(p.w, eventlog.Format(ev, time.Local))
}

// followLogs tails the event logs (and re-reads the macOS power log every
// minute) until ctx ends.
func (e *env) followLogs(ctx context.Context, f eventlog.Filter, p *timelinePrinter, history []eventlog.Event) int {
	fl := eventlog.NewFollower(e.logDirs(eventlog.Filter{Srcs: f.Srcs}), f)
	defer fl.Close()
	_, _ = fl.Poll() // start at the current end of every file
	lastMac := f.Since
	for _, ev := range history {
		if ev.Src == eventlog.SrcMacOS && ev.Time.After(lastMac) {
			lastMac = ev.Time
		}
	}
	nextMac := e.now().Add(logsPmsetInterval)
	for ctx.Err() == nil {
		e.sleep(logsFollowInterval)
		if ctx.Err() != nil {
			break
		}
		evs, err := fl.Poll()
		if err != nil {
			e.errorf("follow: %v", err)
		}
		if now := e.now(); f.WantsSrc(eventlog.SrcMacOS) && !now.Before(nextMac) {
			nextMac = now.Add(logsPmsetInterval)
			mf := f
			mf.Since = lastMac.Add(time.Second) // pmset stamps have 1s resolution
			if mac, err := e.pmsetEvents(ctx, mf); err == nil {
				for _, ev := range mac {
					lastMac = ev.Time
				}
				evs = append(evs, mac...)
				eventlog.SortEvents(evs)
			}
		}
		for _, ev := range evs {
			p.print(ev)
		}
	}
	return exitOK
}

// writeLogBundle writes a .tar.gz for sharing: the log files that cover
// the window, the merged timeline (all levels), the macOS power events,
// `status --json`, `doctor` and the effective config. The ntfy topic is
// redacted from every member.
func (e *env) writeLogBundle(dest string, f eventlog.Filter, now time.Time) int {
	stamp := now.Format("20060102T150405")
	name := "backpack-logs-" + stamp
	if dest == "" {
		dest = name + ".tar.gz"
	} else if fi, err := os.Stat(dest); err == nil && fi.IsDir() {
		dest = filepath.Join(dest, name+".tar.gz")
	}

	cfg, cfgPath, cfgErr := e.loadConfig("")
	topic := cfg.Notify.NtfyTopic
	redact := func(b []byte) []byte { return []byte(redactTopic(string(b), topic)) }

	var manifest strings.Builder
	fmt.Fprintf(&manifest, "backpack log bundle\ncreated: %s\nwindow:  since %s\n", now.Format(time.RFC3339), f.Since.Format(time.RFC3339))
	if len(f.Srcs) > 0 {
		fmt.Fprintf(&manifest, "sources: %s\n", strings.Join(f.Srcs, ","))
	}
	manifest.WriteString("The ntfy topic is replaced by <redacted> in every file.\n\nfiles:\n")

	type member struct {
		name string
		data []byte
	}
	var members []member
	add := func(n string, data []byte) {
		members = append(members, member{n, redact(data)})
		fmt.Fprintf(&manifest, "  %s (%d bytes)\n", n, len(data))
	}
	var problems []string

	// Log files that may hold events in the window, whole.
	for _, d := range []struct{ sub, dir string }{{"engine", e.user.LogDir}, {"daemon", e.sys(paths.DaemonLogDir)}} {
		files, err := eventlog.FilesSince(d.dir, f.Since)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s logs: %v", d.sub, err))
		}
		for _, lf := range files {
			b, err := os.ReadFile(lf.Path)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", lf.Path, err))
				continue
			}
			add(d.sub+"/"+filepath.Base(lf.Path), b)
		}
	}
	// stdio logs (panics, early startup errors): the detached engine's and
	// launchd's for the daemon. Both are unbounded, so only their tails.
	for _, d := range []struct{ sub, path string }{
		{"engine", e.user.EngineLog},
		{"daemon", filepath.Join(e.sys(paths.DaemonLogDir), eventlog.DaemonName+".log")},
	} {
		if b, err := readTail(d.path, bundleEngineLogTail); err == nil {
			add(d.sub+"/"+filepath.Base(d.path), b)
		} else if !errors.Is(err, fs.ErrNotExist) {
			problems = append(problems, fmt.Sprintf("%s: %v", d.path, err))
		}
	}

	// The merged timeline at every level, and the macOS power events.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tf := f
	tf.MinLevel = slog.LevelDebug
	evs, notes := e.collectEvents(ctx, tf)
	problems = append(problems, notes...)
	var timeline, pm bytes.Buffer
	tp := &timelinePrinter{w: &timeline}
	for _, ev := range evs {
		tp.print(ev)
		if ev.Src == eventlog.SrcMacOS {
			raw, _ := ev.Get("raw")
			fmt.Fprintf(&pm, "%s %-9s %v\n", ev.Time.Format("2006-01-02 15:04:05 -0700"), strings.TrimPrefix(ev.Name, "pm."), raw)
		}
	}
	add("timeline.txt", timeline.Bytes())
	if tf.WantsSrc(eventlog.SrcMacOS) {
		add("macos-power.txt", pm.Bytes())
	}

	// status --json (stdout only, so the file stays valid JSON) and doctor,
	// captured from this process.
	stOut, stErr, stCode := e.captureCommand(cmdStatus, "--json")
	add("status.json", stOut)
	if len(stErr) > 0 {
		add("status.stderr.txt", stErr)
	}
	fmt.Fprintf(&manifest, "  (status --json exited %d)\n", stCode)
	drOut, drErr, drCode := e.captureCommand(cmdDoctor)
	add("doctor.txt", append(drOut, drErr...))
	fmt.Fprintf(&manifest, "  (doctor exited %d)\n", drCode)

	var cb bytes.Buffer
	if cfgErr != nil {
		fmt.Fprintf(&cb, "# config %s failed to load: %v\n", cfgPath, cfgErr)
	} else {
		fmt.Fprintf(&cb, "# effective config (%s + defaults + BACKPACK_* overrides)\n", cfgPath)
		if cfg.Notify.NtfyTopic != "" {
			cfg.Notify.NtfyTopic = "<redacted>"
		}
		if err := cfg.WriteTOML(&cb); err != nil {
			fmt.Fprintf(&cb, "# %v\n", err)
		}
	}
	add("config.toml", cb.Bytes())

	if len(problems) > 0 {
		manifest.WriteString("\nproblems:\n")
		for _, p := range problems {
			fmt.Fprintf(&manifest, "  %s\n", p)
		}
	}
	members = append([]member{{"MANIFEST.txt", redact([]byte(manifest.String()))}}, members...)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		hdr := &tar.Header{Name: name + "/" + m.name, Mode: 0o600, Size: int64(len(m.data)), ModTime: now, Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			e.errorf("bundle: %v", err)
			return exitErr
		}
		if _, err := tw.Write(m.data); err != nil {
			e.errorf("bundle: %v", err)
			return exitErr
		}
	}
	if err := tw.Close(); err != nil {
		e.errorf("bundle: %v", err)
		return exitErr
	}
	if err := gz.Close(); err != nil {
		e.errorf("bundle: %v", err)
		return exitErr
	}
	// 0600: logs mention network names, notification titles and pids.
	if err := os.WriteFile(dest, buf.Bytes(), 0o600); err != nil {
		e.errorf("bundle: %v", err)
		return exitErr
	}
	for _, p := range problems {
		e.errorf("note: %s", p)
	}
	e.printf("wrote %s (%d files, %d events since %s; ntfy topic redacted)\n", dest, len(members), len(evs), f.Since.Format("Jan 2 15:04"))
	return exitOK
}

// captureCommand runs a read-only subcommand with its output captured.
func (e *env) captureCommand(cmd func(*env, []string) int, args ...string) (stdout, stderr []byte, code int) {
	var out, errb bytes.Buffer
	sub := *e
	sub.stdout, sub.stderr = &out, &errb
	code = cmd(&sub, args)
	return out.Bytes(), errb.Bytes(), code
}

// readTail returns up to the last n bytes of path.
func readTail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > n {
		if _, err := f.Seek(-n, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}
