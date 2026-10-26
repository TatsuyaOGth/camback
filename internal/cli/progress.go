package cli

import (
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sync"
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
	FileDone(index int, ok bool)
	Done(ok bool)
}

// barProgress shows one bar for all bytes, described as
// "[done/total] NAME +N": the first file in progress and how many others
// are in progress with it.
type barProgress struct {
	out *os.File
	bar *progressbar.ProgressBar // safe for concurrent use

	mu     sync.Mutex // guards the fields below
	files  int
	done   int
	active map[int]string // index -> name of the files in progress
}

func (p *barProgress) Start(files int, bytes int64) {
	p.files = files
	p.active = map[int]string{}
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
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active[i] = it.Dest.Base()
	p.describe()
	return p.bar
}

func (p *barProgress) FileDone(i int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.active, i)
	if ok {
		p.done++
	}
	p.describe()
}

func (p *barProgress) describe() {
	desc := fmt.Sprintf("[%d/%d]", p.done, p.files)
	if len(p.active) > 0 {
		first := slices.Min(slices.Collect(maps.Keys(p.active)))
		desc += " " + p.active[first]
		if n := len(p.active) - 1; n > 0 {
			desc += fmt.Sprintf(" +%d", n)
		}
	}
	p.bar.Describe(desc)
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

// lineProgress prints a line when each file starts, for logs and pipes.
type lineProgress struct {
	out   io.Writer
	mu    sync.Mutex // one line at a time
	files int
}

func (p *lineProgress) Start(files int, _ int64) { p.files = files }

func (p *lineProgress) File(i int, it plan.Item) io.Writer {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.out, "[%d/%d] %s (%s)\n", i+1, p.files, it.File.Rel, humanBytes(it.File.Size))
	return io.Discard
}

func (p *lineProgress) FileDone(int, bool) {}
func (p *lineProgress) Done(bool)          {}
