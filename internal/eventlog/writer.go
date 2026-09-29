package eventlog

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Defaults for Options.
const (
	DefaultMaxFileBytes  = 2 << 20  // 2 MB per file
	DefaultMaxTotalBytes = 50 << 20 // 50 MB per log (active + rotated)
	DefaultMaxAge        = 7 * 24 * time.Hour
)

// rotatedStamp is the timestamp layout in rotated file names:
// engine-20260928T212400.jsonl.
const rotatedStamp = "20060102T150405"

// Ext is the event log file extension.
const Ext = ".jsonl"

// rotatedRE matches a rotated file name for any log name, with an optional
// collision counter: name-20260928T212400.jsonl, name-20260928T212400-2.jsonl.
var rotatedRE = regexp.MustCompile(`^(.+)-\d{8}T\d{6}(?:-\d+)?\.jsonl$`)

// Writer is an io.Writer that appends to DIR/NAME.jsonl, rotates it to
// DIR/NAME-<stamp>.jsonl when the next write would exceed MaxFileBytes, and
// deletes rotated files older than MaxAge or beyond MaxTotalBytes (oldest
// first; the active file counts towards the total but is never deleted).
//
// It is safe for concurrent use. Each Write is written whole to one file,
// so a caller that writes one record per Write (slog handlers do) never
// has a record split across files. A partial last line left by a crash is
// terminated with a newline when the file is reopened, so the next record
// starts on its own line; readers skip the torn line.
type Writer struct {
	dir, name string
	o         Options

	mu     sync.Mutex
	f      *os.File
	size   int64
	closed bool
}

// NewWriter opens (creating if needed) DIR/NAME.jsonl for appending.
func NewWriter(dir, name string, o Options) (*Writer, error) {
	o = o.withDefaults()
	if name == "" || filepath.Base(name) != name {
		return nil, fmt.Errorf("eventlog: bad log name %q", name)
	}
	if err := os.MkdirAll(dir, o.DirMode); err != nil {
		return nil, fmt.Errorf("eventlog: %w", err)
	}
	// The log dir must be a real directory: Chmod below follows symlinks,
	// and the root daemon must not chmod or write through one.
	if fi, err := os.Lstat(dir); err != nil {
		return nil, fmt.Errorf("eventlog: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("eventlog: %s is not a directory (a symlink?)", dir)
	}
	// MkdirAll leaves an existing dir alone and applies the umask to a new
	// one; set the mode explicitly (launchd daemons may run with any umask).
	if err := os.Chmod(dir, o.DirMode); err != nil && !errors.Is(err, fs.ErrPermission) {
		return nil, fmt.Errorf("eventlog: %w", err)
	}
	w := &Writer{dir: dir, name: name, o: o}
	if err := w.open(); err != nil {
		return nil, err
	}
	w.purge()
	return w, nil
}

// Path is the active file.
func (w *Writer) Path() string { return filepath.Join(w.dir, w.name+Ext) }

// open opens the active file. A symlink there is an error (O_NOFOLLOW), as
// is anything but a regular file, so the root daemon never writes to or
// chmods a file someone else pointed it at.
func (w *Writer) open() error {
	f, err := os.OpenFile(w.Path(), os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, w.o.FileMode)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("eventlog: %s is a symlink; refusing to follow it", w.Path())
		}
		return fmt.Errorf("eventlog: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("eventlog: %w", err)
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return fmt.Errorf("eventlog: %s is not a regular file", w.Path())
	}
	if fi.Mode().Perm() != w.o.FileMode {
		_ = f.Chmod(w.o.FileMode)
	}
	size := fi.Size()
	if size > 0 {
		// Terminate a torn last line (crash mid-write) so the next record
		// is not glued onto it.
		var last [1]byte
		if _, err := f.ReadAt(last[:], size-1); err == nil && last[0] != '\n' {
			n, _ := f.Write([]byte{'\n'})
			size += int64(n)
		}
	}
	w.f, w.size = f, size
	return nil
}

// Write appends p, rotating first when p would push the active file past
// MaxFileBytes.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.o.MaxFileBytes {
		if err := w.rotate(); err != nil {
			// Keep logging into the current file rather than losing events.
			fmt.Fprintf(os.Stderr, "eventlog: rotate %s: %v\n", w.Path(), err)
		}
	}
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate syncs and renames the active file, opens a fresh one, and purges.
// Callers hold w.mu.
func (w *Writer) rotate() error {
	_ = w.f.Sync()
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil
	dst := w.rotatedName(w.o.Now())
	if err := os.Rename(w.Path(), dst); err != nil {
		return err
	}
	// The mtime orders rotated files for purging and lets readers skip
	// files older than their window; stamp it with the (possibly fake)
	// clock so both agree with the file name.
	now := w.o.Now()
	_ = os.Chtimes(dst, now, now)
	syncDir(w.dir)
	if err := w.open(); err != nil {
		return err
	}
	w.purge()
	return nil
}

func (w *Writer) rotatedName(now time.Time) string {
	base := filepath.Join(w.dir, w.name+"-"+now.Format(rotatedStamp))
	p := base + Ext
	for i := 2; ; i++ {
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			return p
		}
		p = base + "-" + strconv.Itoa(i) + Ext
	}
}

// rotatedFile is one rotated file of this log.
type rotatedFile struct {
	path  string
	size  int64
	mtime time.Time
}

// purge deletes this log's rotated files that are older than MaxAge, then
// the oldest ones until they plus a full active file fit MaxTotalBytes.
func (w *Writer) purge() {
	ents, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	var files []rotatedFile
	for _, de := range ents {
		m := rotatedRE.FindStringSubmatch(de.Name())
		if m == nil || m[1] != w.name || !de.Type().IsRegular() {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		files = append(files, rotatedFile{filepath.Join(w.dir, de.Name()), fi.Size(), fi.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mtime.Equal(files[j].mtime) {
			return files[i].mtime.Before(files[j].mtime)
		}
		return files[i].path < files[j].path
	})
	// Reserve room for the active file to grow to MaxFileBytes, so the
	// total stays under the cap until the next rotation.
	total := max(w.size, w.o.MaxFileBytes)
	for _, f := range files {
		total += f.size
	}
	cutoff := w.o.Now().Add(-w.o.MaxAge)
	for _, f := range files {
		if total <= w.o.MaxTotalBytes && !f.mtime.Before(cutoff) {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}

// Sync flushes the active file to disk.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	return w.f.Sync()
}

// Close syncs and closes the active file. Later writes fail.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.f == nil {
		return nil
	}
	_ = w.f.Sync()
	err := w.f.Close()
	w.f = nil
	return err
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	d.Close()
}

var _ io.WriteCloser = (*Writer)(nil)
