// Package thermal combines the kernel thermal state (a cgo shim over
// NSProcessInfo.thermalState) with advisory CPU/GPU °C from macmon into a
// contracts.ThermalSource.
package thermal

import (
	"context"
	"fmt"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/providers/macmon"
)

// NSProcessInfoThermalState values.
const (
	nsNominal  = 0
	nsFair     = 1
	nsSerious  = 2
	nsCritical = 3
)

// fromNS maps an NSProcessInfoThermalState to a contracts.ThermalState.
func fromNS(v int) (contracts.ThermalState, error) {
	switch v {
	case nsNominal:
		return contracts.ThermalNominal, nil
	case nsFair:
		return contracts.ThermalFair, nil
	case nsSerious:
		return contracts.ThermalSerious, nil
	case nsCritical:
		return contracts.ThermalCritical, nil
	}
	return contracts.ThermalUnknown, fmt.Errorf("thermal: unknown NSProcessInfoThermalState %d", v)
}

// Options configures New.
type Options struct {
	// MacmonPath is the macmon binary; "" resolves it from
	// paths.MacmonCandidates on each read.
	MacmonPath string
	// DisableMacmon skips temperatures entirely. The root daemon sets it:
	// root must never run a binary from a user-writable path.
	DisableMacmon bool
}

// Source is the combining contracts.ThermalSource.
type Source struct {
	exec contracts.Exec
	opts Options

	// Test seams.
	state func() (contracts.ThermalState, error)
	now   func() time.Time
}

var _ contracts.ThermalSource = (*Source)(nil)

// New returns a ThermalSource: State() for the state, macmon for °C.
func New(exec contracts.Exec, opts Options) contracts.ThermalSource {
	return &Source{exec: exec, opts: opts, state: State, now: time.Now}
}

// Read returns the current reading. It errors only when the state could
// not be read; a macmon failure leaves CPUC/GPUC nil. macmon already drops
// implausible values such as the idle GPU's ~4°C.
func (s *Source) Read(ctx context.Context) (contracts.ThermalReading, error) {
	st, err := s.state()
	r := contracts.ThermalReading{State: st, At: s.now()}
	if err != nil {
		r.State = contracts.ThermalUnknown
		return r, err
	}
	if s.opts.DisableMacmon {
		return r, nil
	}
	if cpu, gpu, err := macmon.Read(ctx, s.exec, s.opts.MacmonPath); err == nil {
		r.CPUC, r.GPUC = cpu, gpu
	}
	return r, nil
}
