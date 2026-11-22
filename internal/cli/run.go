package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/TatsuyaOGth/camback/internal/applog"
	"github.com/TatsuyaOGth/camback/internal/camera"
	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/eject"
	"github.com/TatsuyaOGth/camback/internal/execute"
	"github.com/TatsuyaOGth/camback/internal/exifscan"
	"github.com/TatsuyaOGth/camback/internal/hashing"
	"github.com/TatsuyaOGth/camback/internal/media"
	"github.com/TatsuyaOGth/camback/internal/meta"
	"github.com/TatsuyaOGth/camback/internal/plan"
	"github.com/TatsuyaOGth/camback/internal/source"
	"github.com/TatsuyaOGth/camback/internal/transport"
	"github.com/TatsuyaOGth/camback/internal/transport/sftp"
)

type runOpts struct {
	source   string
	camera   string
	verify   string
	jobs     optInt
	conflict string
	exifChk  string
	noCache  bool
	dryRun   bool
	yes      bool
	eject    bool
	verbose  bool
}

// register adds the run flags to cmd. The same flags live on both the root
// command and "run", so "camback --dry-run" and "camback run --dry-run" work.
func (o *runOpts) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&o.source, "source", "", "use this directory instead of detecting a card")
	f.StringVar(&o.camera, "camera", "", "camera id, only used when no file has EXIF Make/Model")
	f.StringVar(&o.verify, "verify", "", `"hash" (default) or "size"`)
	f.VarP(&o.jobs, "jobs", "j", fmt.Sprintf("files to transfer at the same time, 1-%d (default: defaults.jobs in the config, else %d)", config.MaxJobs, config.DefaultJobs))
	f.StringVar(&o.conflict, "on-conflict", "", `when a different file with the same name is on the destination: "abort" (default), "skip" or "rename" (default: defaults.on_conflict in the config)`)
	f.StringVar(&o.exifChk, "exif-check", "", `media files whose EXIF is read to find the camera: "all" (default) or "sample" (the first and last of each extension in each folder; default: defaults.exif_check in the config)`)
	f.BoolVar(&o.noCache, "no-exif-cache", false, "read every file's EXIF from the card, without using or saving the EXIF cache")
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
	onConflict := cfg.Defaults.OnConflict
	if o.conflict != "" {
		onConflict = o.conflict
	}
	if !config.ValidOnConflict(onConflict) {
		return fmt.Errorf("--on-conflict must be %q, %q or %q", config.OnConflictAbort, config.OnConflictSkip, config.OnConflictRename)
	}
	exifCheck := cfg.Defaults.ExifCheck
	if o.exifChk != "" {
		exifCheck = o.exifChk
	}
	if !config.ValidExifCheck(exifCheck) {
		return fmt.Errorf("--exif-check must be %q or %q", config.ExifCheckAll, config.ExifCheckSample)
	}
	if err := meta.Check(); err != nil {
		return err
	}
	warn := func(s string) { fmt.Fprintln(a.stderr, "Warning:", s) }

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
	var mediaFiles []source.File
	for _, f := range files {
		if mediaSet.IsMedia(f.Rel) {
			mediaFiles = append(mediaFiles, f)
		}
	}
	scanOpt := exifscan.Options{Volume: label, Sample: exifCheck == config.ExifCheckSample, Warn: warn}
	if cfg.Defaults.ExifCacheEnabled() && !o.noCache {
		if dir, err := exifscan.Dir(); err != nil {
			warn(fmt.Sprintf("EXIF cache disabled: %v", err))
		} else {
			scanOpt.CacheDir = dir
		}
	}
	scan, err := exifscan.Scan(ctx, root, mediaFiles, scanOpt)
	if err != nil {
		return err
	}
	res, err := camera.Resolve(cfg, scan.Infos, label, o.camera)
	if err != nil {
		return err
	}
	cam := res.Camera
	fmt.Fprintf(out, "Camera : %s  (%s)\n", cam.ID, describeCamera(res, scan, len(mediaFiles)))
	remote := cfg.Remotes[cam.Remote]
	fmt.Fprintf(out, "Dest   : %s@%s:%d %s\n", remote.User, remote.Host, remote.Port, cam.Dest)

	t, err := dialFunc(ctx, remote, a.prompt)
	if err != nil {
		return err
	}
	defer t.Close()

	p, err := plan.Build(ctx, t, transport.Path(cam.Dest), files, plan.Options{
		IsMedia:    mediaSet.IsMedia,
		OnConflict: onConflict,
		RemoteHash: hashing.NewRemoteHasher(t, warn).Sum,
	})
	if err != nil {
		return err
	}
	printPlan(out, p, o.verbose)

	if n, _ := p.Count(plan.Conflict); n > 0 {
		hint := ""
		if onConflict == config.OnConflictAbort {
			hint = "; to go on without them, use --on-conflict skip or rename"
		}
		return &ExitError{Code: ExitConflict, Err: fmt.Errorf(
			"aborted: %s conflicting with the destination; nothing was transferred%s", plural(n, "file"), hint)}
	}
	if o.dryRun {
		fmt.Fprintln(out, "Dry run: nothing was transferred.")
		return nil
	}

	skipped := p.Filter(plan.Skip)
	todo := p.Filter(plan.New, plan.Rename)
	if len(todo) == 0 && len(skipped) == 0 {
		fmt.Fprintln(out, "Nothing to transfer: everything is already on the destination.")
		return a.maybeEject(root, detected, o.eject)
	}
	var total int64
	for _, it := range todo {
		total += it.File.Size
	}
	if len(todo) > 0 && !o.yes {
		q := fmt.Sprintf("Transfer %s (%s)?", plural(len(todo), "file"), humanBytes(total))
		if len(skipped) > 0 {
			q = fmt.Sprintf("Transfer %s (%s), skipping %d?", plural(len(todo), "file"), humanBytes(total), len(skipped))
		}
		ok, err := a.prompt.Confirm(q)
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
		"new", newN, "new_bytes", newB, "rename", renN, "rename_bytes", renB, "same", sameN, "skip", len(skipped),
		"on_conflict", onConflict, "verify", verify, "jobs", jobs,
		"exif_check", exifCheck, "exif_checked", len(scan.Infos), "exif_cached", scan.Cached, "exif_skipped", scan.Skipped)
	for _, it := range skipped {
		logger.Warn("skipped", "file", it.File.Rel, "dest", it.Dest.String(), "reason", it.Reason)
	}
	if len(todo) == 0 {
		// Only skipped files: the log still records that they are on the card only.
		logger.Info("end", "files", 0, "bytes", 0, "ok", true)
		fmt.Fprintf(out, "Nothing to transfer.\nLog: %s\n", logPath)
		warnSkipped(a.stderr, skipped)
		return a.maybeEject(root, detected, o.eject)
	}

	var fileMode, dirMode fs.FileMode // 0: left to the server
	if remote.FileMode != nil {
		fileMode = *remote.FileMode
	}
	if remote.DirMode != nil {
		dirMode = *remote.DirMode
	}
	result, err := execute.Run(ctx, t, p, execute.Options{
		Verify:   verify,
		Jobs:     jobs,
		FileMode: fileMode,
		DirMode:  dirMode,
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
	warnSkipped(a.stderr, skipped)
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

// describeCamera says how the camera was chosen, for the "Camera :" line.
func describeCamera(res *camera.Result, scan *exifscan.Result, media int) string {
	var notes []string
	if scan.Cached > 0 {
		notes = append(notes, fmt.Sprintf("%d from the EXIF cache", scan.Cached))
	}
	if scan.Skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d not checked: exif_check = sample", scan.Skipped))
	}
	var s string
	switch {
	case res.Method != camera.ByEXIF && scan.Skipped > 0:
		s = fmt.Sprintf("by %s; no checked file has EXIF Make/Model", res.Method)
	case res.Method != camera.ByEXIF:
		s = fmt.Sprintf("by %s; no file has EXIF Make/Model", res.Method)
	case scan.Skipped > 0:
		s = fmt.Sprintf("EXIF matched %d of %d checked out of %s", res.Matched, media-scan.Skipped, plural(media, "media file"))
	default:
		s = fmt.Sprintf("EXIF matched %d of %s", res.Matched, plural(media, "media file"))
	}
	if len(notes) > 0 {
		s += "; " + strings.Join(notes, ", ")
	}
	return s
}
