//go:build darwin && cgo

package thermal

/*
#cgo LDFLAGS: -framework Foundation
int bckp_thermal_state(void);
*/
import "C"

import "github.com/ilias-t/bckp/internal/contracts"

// State reads NSProcessInfo.processInfo.thermalState.
func State() (contracts.ThermalState, error) {
	return fromNS(int(C.bckp_thermal_state()))
}
