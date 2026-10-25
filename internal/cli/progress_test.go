package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/TatsuyaOGth/camback/internal/plan"
	"github.com/TatsuyaOGth/camback/internal/source"
	"github.com/TatsuyaOGth/camback/internal/transport"
)

func item(i int) plan.Item {
	name := fmt.Sprintf("F%02d.JPG", i)
	return plan.Item{File: source.File{Rel: "DCIM/" + name, Size: 100}, Dest: transport.Path("/photo/DCIM/" + name)}
}

// Run with -race: the reporters are used by several transfers at once.
func TestProgressConcurrent(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	var lines bytes.Buffer
	bar := &barProgress{out: devNull}
	for _, p := range []progressReporter{bar, &lineProgress{out: &lines}} {
		p.Start(8, 800)
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				w := p.File(i, item(i))
				w.Write(make([]byte, 100))
				p.FileDone(i, true)
			})
		}
		wg.Wait()
		p.Done(true)
	}
	if bar.done != 8 || len(bar.active) != 0 {
		t.Errorf("bar: done %d, active %v", bar.done, bar.active)
	}
	if n := strings.Count(lines.String(), "\n"); n != 8 {
		t.Errorf("lines: %d, want 8\n%s", n, lines.String())
	}
}

func TestBarDescription(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	p := &barProgress{out: devNull}
	p.Start(10, 1000)
	p.File(3, item(3))
	p.File(1, item(1))
	p.File(2, item(2))
	p.FileDone(2, true)
	if got := p.bar.State().Description; got != "[1/10] F01.JPG +1" {
		t.Errorf("description = %q", got)
	}
	p.FileDone(1, false) // failed files do not count as done
	p.FileDone(3, true)
	if got := p.bar.State().Description; got != "[2/10]" {
		t.Errorf("description = %q", got)
	}
}
