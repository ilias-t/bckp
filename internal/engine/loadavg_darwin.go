//go:build darwin

package engine

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

// sysLoadAvg reads the 1-minute load average from sysctl vm.loadavg:
//
//	struct loadavg { fixpt_t ldavg[3]; long fscale; };
//
// fixpt_t is uint32 and long is 8 bytes on 64-bit macOS, aligned to 8, so
// the raw value is 24 bytes with fscale at offset 16.
func sysLoadAvg() (float64, error) {
	b, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return 0, fmt.Errorf("sysctl vm.loadavg: %w", err)
	}
	if len(b) < 24 {
		return 0, fmt.Errorf("sysctl vm.loadavg: short value (%d bytes)", len(b))
	}
	ld := binary.LittleEndian.Uint32(b[0:4])
	scale := binary.LittleEndian.Uint64(b[16:24])
	if scale == 0 {
		return 0, fmt.Errorf("sysctl vm.loadavg: zero fscale")
	}
	return float64(ld) / float64(scale), nil
}
