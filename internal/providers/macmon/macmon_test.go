package macmon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
)

// Fixtures: macmon_pipe.json was recorded with `macmon pipe -s 1 -i 10`
// (macmon 0.8.2, Apple Silicon). synth_* files are hand-made variants: an idle
// GPU reading 4°C, garbage output, and a sample without temperatures.

const bin = "/opt/homebrew/bin/macmon"

var cmdline = contracts.CommandLine(bin, Args...)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func notRoot(t *testing.T) {
	t.Helper()
	old := geteuid
	geteuid = func() int { return 501 }
	t.Cleanup(func() { geteuid = old })
}

func TestReadRecorded(t *testing.T) {
	notRoot(t)
	ex := fakes.NewExec().On(cmdline, fixture(t, "macmon_pipe.json"))
	cpu, gpu, err := Read(context.Background(), ex, bin)
	if err != nil {
		t.Fatal(err)
	}
	if cpu == nil || *cpu < 41.9 || *cpu > 42.0 {
		t.Errorf("cpu = %v", cpu)
	}
	if gpu == nil || *gpu < 39.6 || *gpu > 39.7 {
		t.Errorf("gpu = %v", gpu)
	}
	if got := ex.Calls(); len(got) != 1 || got[0] != bin+" pipe -s 1 -i 10" {
		t.Errorf("calls = %q", got)
	}
}

func TestReadDropsIdleGPU(t *testing.T) {
	notRoot(t)
	ex := fakes.NewExec().On(cmdline, fixture(t, "synth_macmon_pipe_gpu_idle.json"))
	cpu, gpu, err := Read(context.Background(), ex, bin)
	if err != nil {
		t.Fatal(err)
	}
	if cpu == nil {
		t.Error("cpu should be kept")
	}
	if gpu != nil {
		t.Errorf("gpu 4°C should be dropped, got %v", *gpu)
	}
}

func TestReadErrors(t *testing.T) {
	notRoot(t)
	tests := []struct {
		name string
		ex   *fakes.Exec
	}{
		{"garbage", fakes.NewExec().On(cmdline, fixture(t, "synth_macmon_garbage.txt"))},
		{"no temp", fakes.NewExec().On(cmdline, fixture(t, "synth_macmon_no_temp.json"))},
		{"empty", fakes.NewExec().On(cmdline, "")},
		{"exit 1", fakes.NewExec().OnFail(cmdline, 1, "Error: IOReport failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu, gpu, err := Read(context.Background(), tt.ex, bin)
			if err == nil || cpu != nil || gpu != nil {
				t.Fatalf("got %v %v %v, want error", cpu, gpu, err)
			}
		})
	}
}

func TestParseMissingField(t *testing.T) {
	cpu, gpu, err := Parse([]byte(`{"temp":{"cpu_temp_avg":55.5}}`))
	if err != nil || cpu == nil || *cpu != 55.5 || gpu != nil {
		t.Fatalf("got %v %v %v", cpu, gpu, err)
	}
	cpu, _, err = Parse([]byte(`{"temp":{"cpu_temp_avg":400}}`))
	if err != nil || cpu != nil {
		t.Fatalf("400°C should be implausible: %v %v", cpu, err)
	}
}

// deadlineExec records the context deadline it was called with.
type deadlineExec struct {
	deadline time.Time
	ok       bool
}

func (d *deadlineExec) Run(ctx context.Context, name string, args ...string) (contracts.ExecResult, error) {
	d.deadline, d.ok = ctx.Deadline()
	return contracts.ExecResult{}, ctx.Err()
}

func TestReadIsBounded(t *testing.T) {
	notRoot(t)
	d := &deadlineExec{}
	start := time.Now()
	_, _, _ = Read(context.Background(), d, bin)
	if !d.ok {
		t.Fatal("macmon ran without a deadline")
	}
	if left := d.deadline.Sub(start); left > Timeout+time.Second {
		t.Errorf("deadline %v away, want <= %v", left, Timeout)
	}
}

func TestReadRefusesRoot(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 0 }
	defer func() { geteuid = old }()
	ex := fakes.NewExec()
	if _, _, err := Read(context.Background(), ex, bin); !errors.Is(err, ErrRoot) {
		t.Fatalf("err = %v, want ErrRoot", err)
	}
	if len(ex.Calls()) != 0 {
		t.Errorf("ran %q as root", ex.Calls())
	}
}

type fakeInfo struct {
	fs.FileInfo
	mode fs.FileMode
}

func (f fakeInfo) Mode() fs.FileMode { return f.mode }

func TestResolve(t *testing.T) {
	notRoot(t)
	oldC, oldS := candidates, statFile
	defer func() { candidates, statFile = oldC, oldS }()
	candidates = []string{"/a/macmon", "/b/macmon", "/c/macmon"}
	files := map[string]fs.FileMode{
		"/a/macmon": 0o644, // not executable
		"/b/macmon": 0o755,
	}
	statFile = func(p string) (fs.FileInfo, error) {
		m, ok := files[p]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return fakeInfo{mode: m}, nil
	}
	p, err := Resolve()
	if err != nil || p != "/b/macmon" {
		t.Fatalf("Resolve = %q, %v", p, err)
	}

	// Read with an empty path runs the resolved binary.
	ex := fakes.NewExec().On(contracts.CommandLine("/b/macmon", Args...), fixture(t, "macmon_pipe.json"))
	if cpu, _, err := Read(context.Background(), ex, ""); err != nil || cpu == nil {
		t.Fatalf("Read = %v, %v", cpu, err)
	}

	delete(files, "/b/macmon")
	if _, err := Resolve(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, _, err := Read(context.Background(), ex, ""); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "/c/macmon") {
		t.Fatalf("Read err = %v", err)
	}
}
