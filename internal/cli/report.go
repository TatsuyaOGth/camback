package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/TatsuyaOGth/camback/internal/plan"
)

const newListLimit = 20

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// printPlan writes the plan summary and details.
func printPlan(w io.Writer, p *plan.Plan, verbose bool) {
	row := func(a plan.Action, note string) {
		n, b := p.Count(a)
		size := humanBytes(b)
		if note != "" {
			size = note
		}
		fmt.Fprintf(w, "  %-9s %12s   %s\n", a, plural(n, "file"), size)
	}
	fmt.Fprintln(w)
	row(plan.New, "")
	row(plan.Same, "(skip)")
	row(plan.Rename, "")
	row(plan.Conflict, "")

	if items := p.Filter(plan.New); len(items) > 0 {
		fmt.Fprintln(w, "\nNEW:")
		for i, it := range items {
			if !verbose && i == newListLimit {
				fmt.Fprintf(w, "  ... and %d more (use --verbose to list all)\n", len(items)-newListLimit)
				break
			}
			fmt.Fprintf(w, "  %s  (%s)\n", it.File.Rel, humanBytes(it.File.Size))
		}
	}
	if items := p.Filter(plan.Rename); len(items) > 0 {
		fmt.Fprintln(w, "\nRENAME (changed non-media files; the existing copy is kept):")
		for _, it := range items {
			fmt.Fprintf(w, "  %s -> %s\n", it.File.Rel, it.Dest.Base())
		}
	}
	if items := p.Filter(plan.Conflict); len(items) > 0 {
		fmt.Fprintln(w, "\nCONFLICT (a different file with the same name is already on the destination):")
		for _, it := range items {
			fmt.Fprintf(w, "  %s\n", it.File.Rel)
			fmt.Fprintf(w, "      reason      : %s\n", it.Reason)
			fmt.Fprintf(w, "      card        : %d bytes, %s\n", it.File.Size, it.File.ModTime.Format(time.DateTime))
			if e := it.Existing; e != nil {
				fmt.Fprintf(w, "      destination : %d bytes, %s\n", e.Size, e.ModTime.Local().Format(time.DateTime))
			}
		}
	}
	fmt.Fprintln(w)
}
