package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/TatsuyaOGth/camback/internal/camera"
	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/media"
	"github.com/TatsuyaOGth/camback/internal/meta"
	"github.com/TatsuyaOGth/camback/internal/source"
)

func (a *app) inspectCmd() *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "inspect <path>",
		Short: "Show EXIF Make/Model of media files and which camera they match",
		Long: `inspect reads EXIF Make/Model of the photos and videos under <path> (a card,
a folder or a single file) and shows which [[camera]] in the config each
Make/Model pair matches. Use it to write the "match" entries of a new camera.
The config file is optional for this command.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := a.loadConfig(true)
			if err != nil {
				return err
			}
			return a.inspect(cmd, cfg, args[0], verbose)
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "list every file")
	return cmd
}

func (a *app) inspect(cmd *cobra.Command, cfg *config.Config, target string, verbose bool) error {
	out := a.stdout
	if err := meta.Check(); err != nil {
		return err
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return err
	}

	var exts, excludes []string
	if cfg != nil {
		exts, excludes = cfg.Defaults.MediaExtensions, cfg.Defaults.Exclude
	}
	set := media.NewSet(exts)
	dir := abs
	var rels []string
	if fi.IsDir() {
		files, err := source.Scan(abs, excludes)
		if err != nil {
			return err
		}
		for _, f := range files {
			if set.IsMedia(f.Rel) {
				rels = append(rels, f.Rel)
			}
		}
		if len(rels) == 0 {
			// Distinguish an empty (e.g. freshly formatted) card from a read problem.
			fmt.Fprintf(out, "No photos or videos under %s (%s found, none with a media extension).\n",
				abs, plural(len(files), "other file"))
			if !source.HasDCIM(abs) {
				fmt.Fprintln(out, "There is no DCIM folder: the card may be empty or freshly formatted.")
			}
			return nil
		}
	} else {
		dir = filepath.Dir(abs)
		rels = []string{filepath.Base(abs)}
	}
	infos, err := meta.Read(cmd.Context(), dir, rels)
	if err != nil {
		return err
	}

	matchID := func(i meta.Info) string {
		if cfg == nil || !i.Identified() {
			return "-"
		}
		if c := camera.MatchInfo(cfg, i); c != nil {
			return c.ID
		}
		return "(not in config)"
	}

	if verbose {
		for _, rel := range rels {
			i := infos[rel]
			fmt.Fprintf(out, "%s\tmake=%q\tmodel=%q\tcamera=%s\n", rel, i.Make, i.Model, matchID(i))
		}
		fmt.Fprintln(out)
	}

	type group struct {
		info    meta.Info
		count   int
		example string
	}
	groups := map[meta.Info]*group{}
	for _, rel := range rels {
		i := infos[rel]
		g, ok := groups[i]
		if !ok {
			g = &group{info: i, example: rel}
			groups[i] = g
		}
		g.count++
	}
	list := make([]*group, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].count > list[j].count })

	if cfg == nil {
		fmt.Fprintln(out, "(no config file found; camera column not shown)")
	}
	fmt.Fprintf(out, "%s under %s\n\n", plural(len(rels), "media file"), abs)
	var unknown []meta.Info
	for _, g := range list {
		if !g.info.Identified() {
			fmt.Fprintf(out, "  no EXIF Make/Model    %6d files  (e.g. %s)\n", g.count, g.example)
			continue
		}
		id := matchID(g.info)
		fmt.Fprintf(out, "  make = %q, model = %q\n      %d files, camera: %s  (e.g. %s)\n", g.info.Make, g.info.Model, g.count, id, g.example)
		if id == "(not in config)" || cfg == nil {
			unknown = append(unknown, g.info)
		}
	}
	if len(unknown) > 0 {
		fmt.Fprintln(out, "\nmatch entries for the config file:")
		for _, i := range unknown {
			fmt.Fprintf(out, "  { make = %q, model = %q },\n", i.Make, i.Model)
		}
	}
	return nil
}
