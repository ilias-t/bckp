// Package macmon reads CPU/GPU temperatures from the macmon CLI
// (https://github.com/vladkens/macmon).
//
// macmon lives in a user-writable location (/opt/homebrew), so it is for the
// user-space engine only: Read refuses to run it as root.
package macmon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/paths"
)

// Args is the one-shot invocation: a single sample over a 10ms window.
// Temperatures are sensor reads that do not depend on the interval, and
// on Apple Silicon (macmon 0.8.2) this takes ~0.25s versus ~1.3s for the
// default 1000ms interval.
var Args = []string{"pipe", "-s", "1", "-i", "10"}

// Timeout bounds one macmon run.
const Timeout = 3 * time.Second

// Plausible temperature range in °C. Readings outside it are dropped: the
// idle GPU sensor reads ~4°C.
const (
	MinPlausibleC = 15.0
	MaxPlausibleC = 150.0
)

var (
	// ErrNotFound means no macmon binary was found.
	ErrNotFound = errors.New("macmon: not found")
	// ErrRoot means Read was called as root, which must never run a
	// binary from a user-writable path.
	ErrRoot = errors.New("macmon: refusing to run as root")
)

// Test seams.
var (
	candidates = paths.MacmonCandidates
	geteuid    = os.Geteuid
	statFile   = os.Stat
)

// Resolve returns the first existing, executable macmon candidate.
func Resolve() (string, error) {
	for _, p := range candidates {
		fi, err := statFile(p)
		if err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w in %v", ErrNotFound, candidates)
}

// Read runs macmon once and returns plausible CPU/GPU averages in °C. A
// missing or implausible value is nil without an error; err is set only
// when macmon could not be run or its output could not be parsed. An empty
// path is resolved from paths.MacmonCandidates.
func Read(ctx context.Context, exec contracts.Exec, path string) (cpuC, gpuC *float64, err error) {
	if geteuid() == 0 {
		return nil, nil, ErrRoot
	}
	if path == "" {
		if path, err = Resolve(); err != nil {
			return nil, nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	res, err := exec.Run(ctx, path, Args...)
	if err != nil {
		return nil, nil, fmt.Errorf("macmon: %w", err)
	}
	return Parse(res.Stdout)
}

// sample is the subset of `macmon pipe` JSON we use.
type sample struct {
	Temp *struct {
		CPU *float64 `json:"cpu_temp_avg"`
		GPU *float64 `json:"gpu_temp_avg"`
	} `json:"temp"`
}

// Parse decodes one `macmon pipe` JSON line and applies the plausibility
// filter. It errors on malformed output or when neither temperature field
// is present.
func Parse(out []byte) (cpuC, gpuC *float64, err error) {
	line := bytes.TrimSpace(out)
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i] // one sample per line; use the first
	}
	var s sample
	if err := json.Unmarshal(line, &s); err != nil {
		return nil, nil, fmt.Errorf("macmon: parse output: %w", err)
	}
	if s.Temp == nil || (s.Temp.CPU == nil && s.Temp.GPU == nil) {
		return nil, nil, errors.New("macmon: output has no temperatures")
	}
	return plausible(s.Temp.CPU), plausible(s.Temp.GPU), nil
}

func plausible(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || *v < MinPlausibleC || *v > MaxPlausibleC {
		return nil
	}
	c := *v
	return &c
}
