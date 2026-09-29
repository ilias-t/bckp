// Package power reads AC/battery state (pmset -g batt) and lid/SleepDisabled
// state (ioreg AppleClamshellState). The parsers are pure and stdlib-only
// so the root daemon can reuse them.
package power

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/paths"
)

// Command lines, as argv after the binary.
var (
	BattArgs      = []string{"-g", "batt"}
	ClamshellArgs = []string{"-r", "-k", "AppleClamshellState", "-d", "4"}
)

// Batt is the parsed output of `pmset -g batt`.
type Batt struct {
	// Source is the quoted power source, e.g. "AC Power" or "Battery Power".
	Source string
	// OnAC is true unless the source is "Battery Power".
	OnAC bool
	// HasBattery is false on Macs without an internal battery.
	HasBattery bool
	// Percent is the internal battery charge (0 when HasBattery is false).
	Percent int
	// State is the raw battery state: "charging", "discharging",
	// "charged", "finishing charge", "AC attached", ...
	State string
	// Charging is true for "charging" and "finishing charge".
	Charging bool
}

var (
	sourceRe  = regexp.MustCompile(`drawing from '([^']+)'`)
	batteryRe = regexp.MustCompile(`-InternalBattery-\d+[^\n]*?\s(\d{1,3})%;\s*([^;]+?)\s*(?:;|present:|$)`)
)

// ParseBatt parses `pmset -g batt`, e.g.
//
//	Now drawing from 'AC Power'
//	 -InternalBattery-0 (id=12345678)	100%; charged; 0:00 remaining present: true
func ParseBatt(out string) (Batt, error) {
	m := sourceRe.FindStringSubmatch(out)
	if m == nil {
		return Batt{}, fmt.Errorf("power: pmset: no power source in %q", firstLine(out))
	}
	b := Batt{Source: m[1], OnAC: m[1] != "Battery Power"}
	if m := batteryRe.FindStringSubmatch(out); m != nil {
		pct, err := strconv.Atoi(m[1])
		if err != nil || pct > 100 {
			return Batt{}, fmt.Errorf("power: pmset: bad battery percent %q", m[1])
		}
		b.HasBattery, b.Percent, b.State = true, pct, m[2]
		// Match the state token exactly: "not charging" contains "charging".
		b.Charging = b.State == "charging" || b.State == "finishing charge"
	}
	return b, nil
}

// Clamshell is the parsed IOPMrootDomain lid/sleep state.
type Clamshell struct {
	// LidClosed is AppleClamshellState.
	LidClosed bool
	// SleepDisabled is the kernel's SleepDisabled (pmset disablesleep).
	SleepDisabled bool
	// CausesSleep is AppleClamshellCausesSleep; false while an external
	// display keeps a closed lid awake.
	CausesSleep bool
}

var ioregBoolRe = regexp.MustCompile(`"(AppleClamshellState|AppleClamshellCausesSleep|SleepDisabled)"\s*=\s*(Yes|No)\b`)

// ParseClamshell parses `ioreg -r -k AppleClamshellState -d 4`. It errors
// when AppleClamshellState or SleepDisabled is missing, because callers
// (including the daemon's post-release verification) treat SleepDisabled
// as ground truth. AppleClamshellCausesSleep is optional.
func ParseClamshell(out string) (Clamshell, error) {
	var c Clamshell
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		m := ioregBoolRe.FindStringSubmatch(sc.Text())
		if m == nil || seen[m[1]] {
			continue // first occurrence wins
		}
		seen[m[1]] = true
		v := m[2] == "Yes"
		switch m[1] {
		case "AppleClamshellState":
			c.LidClosed = v
		case "AppleClamshellCausesSleep":
			c.CausesSleep = v
		case "SleepDisabled":
			c.SleepDisabled = v
		}
	}
	if err := sc.Err(); err != nil {
		return Clamshell{}, fmt.Errorf("power: ioreg: %w", err)
	}
	var missing []string
	for _, k := range []string{"AppleClamshellState", "SleepDisabled"} {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return Clamshell{}, fmt.Errorf("power: ioreg: missing %s", strings.Join(missing, ", "))
	}
	return c, nil
}

// Source is the pmset+ioreg contracts.PowerSource.
type Source struct {
	exec contracts.Exec
	now  func() time.Time
}

var _ contracts.PowerSource = (*Source)(nil)

// New returns a PowerSource backed by pmset and ioreg.
func New(exec contracts.Exec) contracts.PowerSource {
	return &Source{exec: exec, now: time.Now}
}

// Read runs pmset and ioreg and combines them.
func (s *Source) Read(ctx context.Context) (contracts.PowerReading, error) {
	res, err := s.exec.Run(ctx, paths.BinPmset, BattArgs...)
	if err != nil {
		return contracts.PowerReading{}, fmt.Errorf("power: %w", err)
	}
	b, err := ParseBatt(string(res.Stdout))
	if err != nil {
		return contracts.PowerReading{}, err
	}
	if !b.HasBattery {
		return contracts.PowerReading{}, fmt.Errorf("power: %w", contracts.ErrNoBattery)
	}
	res, err = s.exec.Run(ctx, paths.BinIoreg, ClamshellArgs...)
	if err != nil {
		return contracts.PowerReading{}, fmt.Errorf("power: %w", err)
	}
	c, err := ParseClamshell(string(res.Stdout))
	if err != nil {
		return contracts.PowerReading{}, err
	}
	return contracts.PowerReading{
		OnAC:          b.OnAC,
		Percent:       b.Percent,
		Charging:      b.Charging,
		LidClosed:     c.LidClosed,
		SleepDisabled: c.SleepDisabled,
		At:            s.now(),
	}, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
