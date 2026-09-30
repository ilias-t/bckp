//go:build !darwin || !cgo

package thermal

import (
	"errors"

	"github.com/ilias-t/bckp/internal/contracts"
)

// State is unavailable without darwin and cgo.
func State() (contracts.ThermalState, error) {
	return contracts.ThermalUnknown, errors.New("thermal: State requires darwin with cgo")
}

// BatteryC is unavailable without darwin and cgo.
func BatteryC() *float64 { return nil }
