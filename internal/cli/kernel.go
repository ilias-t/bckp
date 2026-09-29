package cli

import (
	"context"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/providers/power"
)

// kernelState is the kernel's own view, read directly from ioreg rather
// than from the engine or daemon. nil fields could not be read.
type kernelState struct {
	SleepDisabled        *bool  `json:"sleep_disabled"`
	LidClosed            *bool  `json:"lid_closed"`
	ClamshellCausesSleep *bool  `json:"clamshell_causes_sleep,omitempty"`
	Error                string `json:"error,omitempty"`
}

// readKernel runs the same no-root ioreg query as the power provider and
// parses it with the provider's pure parser.
func readKernel(ctx context.Context, ex contracts.Exec) kernelState {
	res, err := ex.Run(ctx, paths.BinIoreg, power.ClamshellArgs...)
	if err != nil {
		return kernelState{Error: err.Error()}
	}
	c, err := power.ParseClamshell(string(res.Stdout))
	if err != nil {
		return kernelState{Error: err.Error()}
	}
	return kernelState{SleepDisabled: &c.SleepDisabled, LidClosed: &c.LidClosed, ClamshellCausesSleep: &c.CausesSleep}
}

func yesNo(b *bool) string {
	switch {
	case b == nil:
		return "unknown"
	case *b:
		return "Yes"
	}
	return "No"
}

func (k kernelState) summary() string {
	if k.Error != "" && k.SleepDisabled == nil {
		return "unreadable (" + k.Error + ")"
	}
	lid := "lid unknown"
	if k.LidClosed != nil {
		lid = "lid open"
		if *k.LidClosed {
			lid = "lid closed"
		}
	}
	return "SleepDisabled=" + yesNo(k.SleepDisabled) + ", " + lid
}

func (k kernelState) sleepDisabled() bool { return k.SleepDisabled != nil && *k.SleepDisabled }
