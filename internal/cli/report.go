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
	row(plan.Same, "(already there)")
	row(plan.Rename, "")
	row(plan.Conflict, "")
	if n, _ := p.Count(plan.Skip); n > 0 {
		row(plan.Skip, "(not transferred)")
	}

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
		fmt.Fprintln(w, "\nRENAME (changed files; the existing copy is kept):")
		for _, it := range items {
			fmt.Fprintf(w, "  %s -> %s\n", it.File.Rel, it.Dest.Base())
		}
	}
	conflicts(w, p.Filter(plan.Conflict), "CONFLICT (a different file with the same name is already on the destination):")
	conflicts(w, p.Filter(plan.Skip), "SKIP (conflicting files left on the card only):")
	fmt.Fprintln(w)
}

// conflicts lists CONFLICT or SKIP items with what differs.
func conflicts(w io.Writer, items []plan.Item, title string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintln(w, "\n"+title)
	for _, it := range items {
		fmt.Fprintf(w, "  %s\n", it.File.Rel)
		fmt.Fprintf(w, "      reason      : %s\n", it.Reason)
		fmt.Fprintf(w, "      card        : %d bytes, %s\n", it.File.Size, it.File.ModTime.Format(time.DateTime))
		if e := it.Existing; e != nil {
			fmt.Fprintf(w, "      destination : %d bytes, %s\n", e.Size, e.ModTime.Local().Format(time.DateTime))
		}
	}
}

// warnSkipped reminds that the skipped files are only on the card.
func warnSkipped(w io.Writer, items []plan.Item) {
	if len(items) == 0 {
		return
	}
	where := "they are"
	if len(items) == 1 {
		where = "it is"
	}
	fmt.Fprintf(w, "Warning: %s skipped because of conflicts; %s only on the card:\n",
		plural(len(items), "file"), where)
	for _, it := range items {
		fmt.Fprintf(w, "  %s\n", it.File.Rel)
	}
}
