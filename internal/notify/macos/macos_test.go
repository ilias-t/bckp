package macos

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
)

// argvExec records the argv of each run (fakes.Exec keys on a joined
// string, which cannot show argument boundaries).
type argvExec struct {
	name string
	args []string
	err  error
}

func (a *argvExec) Run(_ context.Context, name string, args ...string) (contracts.ExecResult, error) {
	a.name, a.args = name, slices.Clone(args)
	return contracts.ExecResult{}, a.err
}

func TestSendPassesTextAsArgv(t *testing.T) {
	ex := &argvExec{}
	evil := `x" & (do shell script "touch /tmp/pwned") & "`
	body := "-e line1\nline2 CPU 92°C \\ \"q\"\x00"
	if err := New(ex).Send(context.Background(), contracts.Notification{Title: evil, Body: body}); err != nil {
		t.Fatal(err)
	}
	if ex.name != "/usr/bin/osascript" {
		t.Errorf("name = %q", ex.name)
	}
	want := []string{
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		"--", evil, "-e line1\nline2 CPU 92°C \\ \"q\"",
	}
	if !slices.Equal(ex.args, want) {
		t.Errorf("args =\n%q\nwant\n%q", ex.args, want)
	}
	// The script itself is constant: user text never reaches it.
	for _, a := range ex.args[:6] {
		if a == evil {
			t.Error("title leaked into the script")
		}
	}
}

func TestSendDefaultTitleAndErrors(t *testing.T) {
	ex := fakes.NewExec()
	cmd := contracts.CommandLine(BinOsascript, Args(DefaultTitle, "body")...)
	ex.On(cmd, "")
	if err := New(ex).Send(context.Background(), contracts.Notification{Body: "body"}); err != nil {
		t.Fatal(err)
	}
	if ex.CallCount(cmd) != 1 {
		t.Errorf("calls = %q", ex.Calls())
	}

	boom := errors.New("boom")
	if err := New(&argvExec{err: boom}).Send(context.Background(), contracts.Notification{Title: "x"}); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if err := New(&argvExec{}).Send(context.Background(), contracts.Notification{}); err == nil {
		t.Error("empty notification accepted")
	}
}
