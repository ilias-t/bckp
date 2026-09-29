package eventlog

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// Follower tails every log file in a set of dirs, like `tail -F`. Files
// are tracked by identity (device and inode), not name, so a file keeps
// being read after it is rotated away, and files rotated between two polls
// (created and renamed in between) are read from their start. Call Poll
// periodically.
type Follower struct {
	dirs   []string
	filter Filter
	files  map[fileID]*followed
	primed bool
}

type fileID struct{ dev, ino uint64 }

type followed struct {
	offset  int64
	partial []byte
	seen    bool
}

// NewFollower returns a follower for dirs. The first Poll starts at the
// current end of the existing files and returns nothing; files created
// afterwards are read from their start.
func NewFollower(dirs []string, f Filter) *Follower {
	return &Follower{dirs: dirs, filter: f, files: map[fileID]*followed{}}
}

// Poll returns the matching events written since the previous Poll, in
// time order. Errors for individual dirs or files are joined; the events
// that could be read are still returned.
func (fl *Follower) Poll() ([]Event, error) {
	var (
		out  []Event
		errs []error
		st   Stats
	)
	for _, s := range fl.files {
		s.seen = false
	}
	for _, dir := range fl.dirs {
		files, err := Files(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		for _, lf := range files {
			evs, err := fl.read(lf.Path, &st)
			if err != nil {
				errs = append(errs, err)
			}
			out = append(out, evs...)
		}
	}
	for id, s := range fl.files {
		if !s.seen { // purged
			delete(fl.files, id)
		}
	}
	fl.primed = true
	SortEvents(out)
	return out, errors.Join(errs...)
}

func (fl *Follower) read(path string, st *Stats) ([]Event, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // rotated or purged since the listing
		}
		return nil, err
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("eventlog: follow: no inode for " + path)
	}
	id := fileID{uint64(sys.Dev), uint64(sys.Ino)}
	s := fl.files[id]
	if s == nil {
		s = &followed{}
		if !fl.primed {
			s.offset = fi.Size()
		}
		fl.files[id] = s
	}
	s.seen = true
	if fi.Size() < s.offset { // truncated, or a reused inode
		s.offset, s.partial = 0, nil
	}
	if fi.Size() == s.offset {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	s.offset += int64(len(b))
	buf := append(s.partial, b...)
	cut := bytes.LastIndexByte(buf, '\n')
	if cut < 0 {
		s.partial = buf
		return nil, err
	}
	s.partial = append([]byte(nil), buf[cut+1:]...)
	evs, rerr := readEvents(bytes.NewReader(buf[:cut+1]), fl.filter, st)
	return evs, errors.Join(err, rerr)
}

// Close releases the follower's state.
func (fl *Follower) Close() { clear(fl.files) }
