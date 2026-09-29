//go:build !darwin

package engine

import "errors"

func sysLoadAvg() (float64, error) { return 0, errors.New("load average: unsupported platform") }
