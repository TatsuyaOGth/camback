package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// errNotInteractive is returned when input is needed but stdin is not a terminal.
var errNotInteractive = errors.New("input required but stdin is not a terminal")

// termPrompter reads answers from the terminal and writes prompts to stderr,
// so that stdout stays clean for the plan output.
//
// Every prompt returns as soon as ctx is cancelled (Ctrl-C). Input is read
// without buffering, so a line read for Confirm never swallows what the user
// types next for Password.
type termPrompter struct {
	ctx context.Context
	in  *os.File
	out io.Writer
}

func newTermPrompter(ctx context.Context, in *os.File, out io.Writer) *termPrompter {
	return &termPrompter{ctx: ctx, in: in, out: out}
}

func (p *termPrompter) interactive() bool { return term.IsTerminal(int(p.in.Fd())) }

// await runs a blocking read in a goroutine and stops waiting when ctx is
// cancelled. The abandoned read is harmless: once ctx is cancelled every
// later prompt returns immediately, and the program only cleans up and exits.
// (await is a generic function: T is filled in by each caller.)
func await[T any](ctx context.Context, read func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1) // buffered, so the goroutine never blocks after we stop waiting
	go func() {
		v, err := read()
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// readLine reads one line byte by byte (no read-ahead).
func (p *termPrompter) readLine() (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := p.in.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return b.String(), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && b.Len() > 0 {
				return b.String(), nil
			}
			return "", err
		}
	}
}

func (p *termPrompter) line() (string, error) {
	s, err := await(p.ctx, p.readLine)
	if err != nil {
		fmt.Fprintln(p.out)
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// Password reads a secret without echo.
func (p *termPrompter) Password(prompt string) (string, error) {
	if !p.interactive() {
		return "", errNotInteractive
	}
	fd := int(p.in.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	fmt.Fprint(p.out, prompt)
	b, err := await(p.ctx, func() ([]byte, error) { return term.ReadPassword(fd) })
	fmt.Fprintln(p.out)
	if p.ctx.Err() != nil {
		// ReadPassword turned echo off and is still waiting; turn it back on.
		term.Restore(fd, state)
	}
	return string(b), err
}

// Confirm asks a yes/no question; anything but y/yes is "no".
func (p *termPrompter) Confirm(prompt string) (bool, error) {
	if !p.interactive() {
		return false, errNotInteractive
	}
	fmt.Fprint(p.out, prompt+" (y/N): ")
	s, err := p.line()
	if err != nil {
		return false, err
	}
	s = strings.ToLower(s)
	return s == "y" || s == "yes", nil
}

// Choose shows numbered options and returns the chosen index.
func (p *termPrompter) Choose(prompt string, options []string) (int, error) {
	if !p.interactive() {
		return 0, errNotInteractive
	}
	fmt.Fprintln(p.out, prompt)
	for i, o := range options {
		fmt.Fprintf(p.out, "  %d: %s\n", i+1, o)
	}
	for {
		fmt.Fprintf(p.out, "Select 1-%d: ", len(options))
		s, err := p.line()
		if err != nil {
			return 0, err
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
	}
}
