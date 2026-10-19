package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/schollz/progressbar/v3"
	"golang.org/x/term"

	"github.com/TatsuyaOGth/camback/internal/plan"
)

// newProgress returns a progress bar on a terminal, plain lines otherwise.
func newProgress(out io.Writer) progressReporter {
	if f, ok := out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return &barProgress{out: f}
	}
	return &lineProgress{out: out}
}

type progressReporter interface {
	Start(files int, bytes int64)
	File(index int, it plan.Item) io.Writer
	Done(ok bool)
}

type barProgress struct {
	out   *os.File
	bar   *progressbar.ProgressBar
	files int
}

func (p *barProgress) Start(files int, bytes int64) {
	p.files = files
	p.bar = progressbar.NewOptions64(bytes,
		progressbar.OptionSetWriter(p.out),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(30),
		progressbar.OptionThrottle(100*time.Millisecond),
		progressbar.OptionSetPredictTime(true),
		progressbar.OptionFullWidth(),
	)
}

func (p *barProgress) File(i int, it plan.Item) io.Writer {
	p.bar.Describe(fmt.Sprintf("[%d/%d] %s", i+1, p.files, it.Dest.Base()))
	return p.bar
}

// Done fills the bar only on success; on failure it stays where it stopped.
func (p *barProgress) Done(ok bool) {
	if ok {
		p.bar.Finish()
	} else {
		p.bar.Exit()
	}
	fmt.Fprintln(p.out)
}

type lineProgress struct {
	out   io.Writer
	files int
}

func (p *lineProgress) Start(files int, _ int64) { p.files = files }

func (p *lineProgress) File(i int, it plan.Item) io.Writer {
	fmt.Fprintf(p.out, "[%d/%d] %s (%s)\n", i+1, p.files, it.File.Rel, humanBytes(it.File.Size))
	return io.Discard
}

func (p *lineProgress) Done(bool) {}
