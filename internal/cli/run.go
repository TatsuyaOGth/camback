package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/TatsuyaOGth/camback/internal/applog"
	"github.com/TatsuyaOGth/camback/internal/camera"
	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/eject"
	"github.com/TatsuyaOGth/camback/internal/execute"
	"github.com/TatsuyaOGth/camback/internal/hashing"
	"github.com/TatsuyaOGth/camback/internal/media"
	"github.com/TatsuyaOGth/camback/internal/meta"
	"github.com/TatsuyaOGth/camback/internal/plan"
	"github.com/TatsuyaOGth/camback/internal/source"
	"github.com/TatsuyaOGth/camback/internal/transport"
	"github.com/TatsuyaOGth/camback/internal/transport/sftp"
)

type runOpts struct {
	source  string
	camera  string
	verify  string
	jobs    optInt
	dryRun  bool
	yes     bool
	eject   bool
	verbose bool
}

// register adds the run flags to cmd. The same flags live on both the root
// command and "run", so "camback --dry-run" and "camback run --dry-run" work.
func (o *runOpts) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&o.source, "source", "", "use this directory instead of detecting a card")
	f.StringVar(&o.camera, "camera", "", "camera id, only used when no file has EXIF Make/Model")
	f.StringVar(&o.verify, "verify", "", `"hash" (default) or "size"`)
	f.VarP(&o.jobs, "jobs", "j", fmt.Sprintf("files to transfer at the same time, 1-%d (default: defaults.jobs in the config, else %d)", config.MaxJobs, config.DefaultJobs))
	f.BoolVar(&o.dryRun, "dry-run", false, "show the plan and exit")
	f.BoolVarP(&o.yes, "yes", "y", false, "do not ask before transferring")
	f.BoolVar(&o.eject, "eject", false, "eject the card after a successful run without asking")
	f.BoolVarP(&o.verbose, "verbose", "v", false, "list every NEW file")
}

// optInt is an int flag that remembers whether it was given, so that an
// explicit 0 is rejected instead of meaning "not given".
type optInt struct {
	n   int
	set bool
}

func (o *optInt) String() string {
	if !o.set {
		return ""
	}
	return strconv.Itoa(o.n)
}

func (o *optInt) Set(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	o.n, o.set = n, true
	return nil
}

func (o *optInt) Type() string { return "int" }

// dialFunc is replaced in tests.
var dialFunc = func(ctx context.Context, r config.Remote, p sftp.Prompter) (transport.Transport, error) {
	return sftp.Dial(ctx, r, p)
}

func (a *app) run(ctx context.Context, o *runOpts) error {
	out := a.stdout
	cfg, cfgPath, err := a.loadConfig(false)
	if err != nil {
		return err
	}
	verify := cfg.Defaults.Verify
	if o.verify != "" {
		verify = o.verify
	}
	if verify != config.VerifyHash && verify != config.VerifySize {
		return fmt.Errorf("--verify must be %q or %q", config.VerifyHash, config.VerifySize)
	}
	jobs := cfg.Defaults.Jobs
	if o.jobs.set {
		jobs = o.jobs.n
	}
	if jobs < 1 || jobs > config.MaxJobs {
		return fmt.Errorf("--jobs must be between 1 and %d", config.MaxJobs)
	}
	if err := meta.Check(); err != nil {
		return err
	}

	root, label, detected, err := a.pickSource(o.source)
	if err != nil {
		return err
	}
	files, err := source.Scan(root, cfg.Defaults.Exclude)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Config : %s\n", cfgPath)
	fmt.Fprintf(out, "Source : %s  (volume %q, %s)\n", root, label, plural(len(files), "file"))
	if len(files) == 0 {
		fmt.Fprintln(out, "Nothing on the card.")
		return nil
	}

	mediaSet := media.NewSet(cfg.Defaults.MediaExtensions)
	var mediaRels []string
	for _, f := range files {
		if mediaSet.IsMedia(f.Rel) {
			mediaRels = append(mediaRels, f.Rel)
		}
	}
	infos, err := meta.Read(ctx, root, mediaRels)
	if err != nil {
		return err
	}
	res, err := camera.Resolve(cfg, infos, label, o.camera)
	if err != nil {
		return err
	}
	cam := res.Camera
	switch res.Method {
	case camera.ByEXIF:
		fmt.Fprintf(out, "Camera : %s  (EXIF matched %d of %s)\n", cam.ID, res.Matched, plural(len(mediaRels), "media file"))
	default:
		fmt.Fprintf(out, "Camera : %s  (by %s; no file has EXIF Make/Model)\n", cam.ID, res.Method)
	}
	remote := cfg.Remotes[cam.Remote]
	fmt.Fprintf(out, "Dest   : %s@%s:%d %s\n", remote.User, remote.Host, remote.Port, cam.Dest)

	t, err := dialFunc(ctx, remote, a.prompt)
	if err != nil {
		return err
	}
	defer t.Close()

	warn := func(s string) { fmt.Fprintln(a.stderr, "Warning:", s) }
	p, err := plan.Build(ctx, t, transport.Path(cam.Dest), files, plan.Options{
		IsMedia:    mediaSet.IsMedia,
		RemoteHash: hashing.NewRemoteHasher(t, warn).Sum,
	})
	if err != nil {
		return err
	}
	printPlan(out, p, o.verbose)

	if n, _ := p.Count(plan.Conflict); n > 0 {
		return &ExitError{Code: ExitConflict, Err: fmt.Errorf(
			"aborted: %s conflicting with the destination; nothing was transferred", plural(n, "file"))}
	}
	if o.dryRun {
		fmt.Fprintln(out, "Dry run: nothing was transferred.")
		return nil
	}

	todo := p.Filter(plan.New, plan.Rename)
	if len(todo) == 0 {
		fmt.Fprintln(out, "Nothing to transfer: everything is already on the destination.")
		return a.maybeEject(root, detected, o.eject)
	}
	var total int64
	for _, it := range todo {
		total += it.File.Size
	}
	if !o.yes {
		ok, err := a.prompt.Confirm(fmt.Sprintf("Transfer %s (%s)?", plural(len(todo), "file"), humanBytes(total)))
		if errors.Is(err, errNotInteractive) {
			return fmt.Errorf("%w; pass --yes to transfer without asking", err)
		}
		if err != nil {
			return err
		}
		if !ok {
			return &ExitError{Code: ExitCanceled, Err: errors.New("cancelled; nothing was transferred")}
		}
	}

	logger, logPath, closeLog, err := applog.Open()
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer closeLog()
	newN, newB := p.Count(plan.New)
	renN, renB := p.Count(plan.Rename)
	sameN, _ := p.Count(plan.Same)
	logger.Info("start", "source", root, "volume", label, "camera", cam.ID, "method", string(res.Method),
		"remote", fmt.Sprintf("%s@%s:%d", remote.User, remote.Host, remote.Port), "dest", cam.Dest,
		"new", newN, "new_bytes", newB, "rename", renN, "rename_bytes", renB, "same", sameN, "verify", verify, "jobs", jobs)

	result, err := execute.Run(ctx, t, p, execute.Options{
		Verify:   verify,
		Jobs:     jobs,
		Logger:   logger,
		Progress: newProgress(a.stderr),
		Warn:     warn,
	})
	if result != nil {
		logger.Info("end", "files", result.Files, "bytes", result.Bytes, "verify_method", result.VerifyMethod, "ok", err == nil)
	}
	if err != nil {
		done := 0
		if result != nil {
			done = result.Files
		}
		fmt.Fprintf(a.stderr, "Transferred %d of %d files before stopping. Log: %s\n", done, len(todo), logPath)
		switch {
		case errors.Is(err, context.Canceled):
			return &ExitError{Code: ExitCanceled, Err: errors.New("interrupted")}
		case errors.Is(err, execute.ErrConflict):
			return &ExitError{Code: ExitConflict, Err: err}
		}
		return err
	}
	fmt.Fprintf(out, "Done: %s (%s), verified by %s.\nLog: %s\n",
		plural(result.Files, "file"), humanBytes(result.Bytes), result.VerifyMethod, logPath)
	return a.maybeEject(root, detected, o.eject)
}

// pickSource returns the card root, its volume label, and whether it was
// detected automatically (only detected cards are offered for ejection).
func (a *app) pickSource(flag string) (root, label string, detected bool, err error) {
	if flag != "" {
		abs, err := filepath.Abs(flag)
		if err != nil {
			return "", "", false, err
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return "", "", false, err
		}
		if !fi.IsDir() {
			return "", "", false, fmt.Errorf("--source %s is not a directory", abs)
		}
		return abs, source.VolumeLabel(abs), false, nil
	}
	cands := source.Detect()
	switch len(cands) {
	case 0:
		return "", "", false, errors.New("no memory card with a DCIM folder was found; use --source <path>")
	case 1:
		return cands[0].Root, cands[0].Label, true, nil
	}
	opts := make([]string, len(cands))
	for i, c := range cands {
		opts[i] = fmt.Sprintf("%s  (%s)", c.Root, c.Label)
	}
	i, err := a.prompt.Choose("Several memory cards were found:", opts)
	if err != nil {
		return "", "", false, fmt.Errorf("choose a card: %w; use --source <path>", err)
	}
	return cands[i].Root, cands[i].Label, true, nil
}

func (a *app) maybeEject(root string, detected, force bool) error {
	if !detected {
		return nil
	}
	if !force {
		ok, err := a.prompt.Confirm("Eject " + root + "?")
		if err != nil || !ok {
			return nil // not ejecting is never an error
		}
	}
	if err := eject.Eject(root); err != nil {
		fmt.Fprintln(a.stderr, "Warning: eject failed:", err)
		return nil
	}
	fmt.Fprintln(a.stdout, "Ejected", root)
	return nil
}
