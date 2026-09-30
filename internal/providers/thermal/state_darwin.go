//go:build darwin && cgo

package thermal

/*
#cgo LDFLAGS: -framework Foundation -framework IOKit
int bckp_thermal_state(void);
int bckp_battery_temps(double *out, int max);
*/
import "C"

import "github.com/ilias-t/bckp/internal/contracts"

// State reads NSProcessInfo.processInfo.thermalState.
func State() (contracts.ThermalState, error) {
	return fromNS(int(C.bckp_thermal_state()))
}

// BatteryC reads the battery's fuel-gauge temperature sensors and returns
// the hottest plausible one, or nil when there is none (no battery, or the
// private API changed).
func BatteryC() *float64 {
	var buf [16]C.double
	n := int(C.bckp_battery_temps(&buf[0], C.int(len(buf))))
	temps := make([]float64, n)
	for i := range n {
		temps[i] = float64(buf[i])
	}
	return hottestPlausible(temps)
}
