package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/humanize"
	"github.com/ilias-t/bckp/internal/paths"
)

// JournalVersion is the current restore-journal format.
const JournalVersion = 1

// Journal is the network restore journal (paths.User.RestoreJournal). The
// engine writes it before its first network change and rewrites it before
// each further kind of change, so a crash at any point can be undone by
// RestoreStaleJournal. Only changes whose flag is set are undone.
type Journal struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	PID       int       `json:"pid"`
	// Service is the hotspot network service (config network.service).
	Service string `json:"service"`
	// ServiceOrder is the original network service order.
	ServiceOrder []string `json:"service_order"`
	// WiFiPower is the original Wi-Fi power; nil when it could not be read
	// (Wi-Fi is then never touched).
	WiFiPower *bool `json:"wifi_power"`

	// Reordered: the engine changed the service order.
	Reordered bool `json:"reordered,omitempty"`
	// WiFiChanged: the engine changed Wi-Fi power.
	WiFiChanged bool `json:"wifi_changed,omitempty"`
	// ServiceDisabled: the engine disabled Service (recovery toggle in
	// progress).
	ServiceDisabled bool `json:"service_disabled,omitempty"`
	// WiFiNudge: a Wi-Fi nudge is in progress (Wi-Fi may be off between
	// its off and on phases). Restoring puts Wi-Fi back to WiFiPower, or
	// on when that is unknown.
	WiFiNudge bool `json:"wifi_nudge,omitempty"`
}

// readJournal loads the journal at path. It returns nil, nil when there is
// none.
func readJournal(path string) (*Journal, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("restore journal: %w", err)
	}
	var j Journal
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("restore journal %s: %w", path, err)
	}
	if j.Version != JournalVersion {
		return nil, fmt.Errorf("restore journal %s: unsupported version %d", path, j.Version)
	}
	return &j, nil
}

// writeJournal atomically writes j to path (mode 0600), creating the
// directory (0700) if needed.
func writeJournal(path string, j Journal) error {
	if path == "" {
		return errors.New("restore journal: no path configured")
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("restore journal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("restore journal: %w", err)
	}
	f, err := os.CreateTemp(dir, ".restore-*.json")
	if err != nil {
		return fmt.Errorf("restore journal: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("restore journal: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("restore journal: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("restore journal: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("restore journal: %w", err)
	}
	return nil
}

// applyJournal undoes the changes flagged in j. It attempts every step and
// returns all failures joined.
func applyJournal(ctx context.Context, n contracts.Network, j Journal) error {
	var errs []error
	if j.ServiceDisabled && j.Service != "" {
		if err := n.SetServiceEnabled(ctx, j.Service, true); err != nil {
			errs = append(errs, fmt.Errorf("re-enable %q: %w", j.Service, err))
		}
	}
	if j.Reordered && len(j.ServiceOrder) > 0 {
		cur, err := n.ServiceOrder(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("read service order: %w", err))
		} else if want := mergeOrder(j.ServiceOrder, cur); !slices.Equal(want, cur) {
			if err := n.SetServiceOrder(ctx, want); err != nil {
				errs = append(errs, fmt.Errorf("restore service order: %w", err))
			}
		}
	}
	if j.WiFiChanged || j.WiFiNudge {
		want, ok := true, j.WiFiNudge
		if j.WiFiPower != nil {
			want, ok = *j.WiFiPower, true
		}
		if ok {
			if err := n.SetWiFiPower(ctx, want); err != nil {
				errs = append(errs, fmt.Errorf("restore wifi %s: %w", humanize.OnOff(want), err))
			}
		}
	}
	return errors.Join(errs...)
}

// RestoreStaleJournal re-applies and deletes a journal left behind by an
// engine that did not exit cleanly. The CLI calls it from start, stop and
// doctor when no engine is running. restored is true when a journal existed
// and was fully applied and removed; on failure the journal is kept for a
// later retry. An unreadable journal is renamed to restore.json.bad and
// reported as an error, so it cannot block every later start.
func RestoreStaleJournal(ctx context.Context, network contracts.Network, p paths.User) (restored bool, err error) {
	j, err := readJournal(p.RestoreJournal)
	if err != nil {
		if _, statErr := os.Stat(p.RestoreJournal); statErr == nil {
			bad := p.RestoreJournal + ".bad"
			if renameErr := os.Rename(p.RestoreJournal, bad); renameErr == nil {
				return false, fmt.Errorf("%w (moved aside to %s)", err, bad)
			}
		}
		return false, err
	}
	if j == nil {
		return false, nil
	}
	if err := applyJournal(ctx, network, *j); err != nil {
		return false, fmt.Errorf("restore journal %s: %w", p.RestoreJournal, err)
	}
	if err := os.Remove(p.RestoreJournal); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("restore journal: %w", err)
	}
	return true, nil
}

// mergeOrder returns the original order restricted to services that still
// exist, followed by services that appeared since (in their current
// relative order). The result is always a permutation of current, as
// SetServiceOrder requires.
func mergeOrder(original, current []string) []string {
	out := make([]string, 0, len(current))
	for _, s := range original {
		if slices.Contains(current, s) && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, s := range current {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// hotspotOrder moves service directly ahead of the Wi-Fi service (or to the
// front when there is no Wi-Fi service). changed is false when service
// already ranks above Wi-Fi.
func hotspotOrder(order []string, service string) (out []string, changed bool, err error) {
	si := slices.Index(order, service)
	if si < 0 {
		return nil, false, fmt.Errorf("network service %q not found in %v", service, order)
	}
	wi := slices.IndexFunc(order, isWiFiService)
	if wi < 0 {
		wi = 0
	}
	if si <= wi {
		return slices.Clone(order), false, nil
	}
	out = slices.Delete(slices.Clone(order), si, si+1)
	out = slices.Insert(out, wi, service)
	return out, true, nil
}

func isWiFiService(name string) bool {
	n := strings.ToLower(name)
	return n == "wi-fi" || strings.Contains(n, "wi-fi") || strings.Contains(n, "wifi") || strings.Contains(n, "airport")
}
