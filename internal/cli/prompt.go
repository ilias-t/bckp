package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// prompter asks questions on the terminal. Every prompt in a run shares
// one prompter (and so one bufio.Reader over stdin): a reader per question
// would read ahead and swallow the answers that follow.
//
// Callers must only prompt when env.isTTY() is true (stdin and stdout are
// both terminals) and --yes was not given; a caller without a terminal
// must never block on stdin.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

// prompter returns the run's shared prompter.
func (e *env) prompter() *prompter {
	if e.prompt == nil {
		e.prompt = &prompter{in: bufio.NewReader(e.stdin), out: e.stdout}
	}
	return e.prompt
}

// line prints question and returns the trimmed answer. End of input
// without an answer is errPromptAborted.
func (p *prompter) line(question string) (string, error) {
	fmt.Fprint(p.out, question+" ")
	s, err := p.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || s == "") {
		fmt.Fprintln(p.out)
		return "", errPromptAborted
	}
	return strings.TrimSpace(s), nil
}

// choose asks until parse accepts the answer; Enter picks def. It gives
// up after maxPromptTries invalid answers.
func (p *prompter) choose(question, def string, parse func(string) (string, error)) (string, error) {
	for try := 1; ; try++ {
		ans, err := p.line(question)
		if err != nil {
			return "", err
		}
		if ans == "" {
			return def, nil
		}
		v, err := parse(ans)
		if err == nil {
			return v, nil
		}
		if try == maxPromptTries {
			return "", fmt.Errorf("%v; nothing started", err)
		}
		fmt.Fprintf(p.out, "  %v\n", err)
	}
}

// yesNo asks a [Y/n] (def true) or [y/N] question.
func (p *prompter) yesNo(question string, def bool) (bool, error) {
	hint, dv := "[y/N]", "n"
	if def {
		hint, dv = "[Y/n]", "y"
	}
	v, err := p.choose(question+" "+hint+":", dv, func(s string) (string, error) {
		switch strings.ToLower(s) {
		case "y", "yes":
			return "y", nil
		case "n", "no":
			return "n", nil
		}
		return "", fmt.Errorf("please answer y or n")
	})
	return v == "y", err
}
