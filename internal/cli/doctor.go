package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/TatsuyaOGth/camback/internal/config"
	"github.com/TatsuyaOGth/camback/internal/execute"
	"github.com/TatsuyaOGth/camback/internal/hashing"
	"github.com/TatsuyaOGth/camback/internal/meta"
	"github.com/TatsuyaOGth/camback/internal/source"
	"github.com/TatsuyaOGth/camback/internal/transport"
	"github.com/TatsuyaOGth/camback/internal/transport/sftp"
)

// dialTraceFunc is replaced in tests.
var dialTraceFunc = func(ctx context.Context, r config.Remote, p sftp.Prompter, tr *sftp.Trace) (transport.Transport, error) {
	c, err := sftp.DialTrace(ctx, r, p, tr)
	if err != nil {
		return nil, err // not a nil *Client inside a non-nil interface
	}
	return c, nil
}

func (a *app) doctorCmd() *cobra.Command {
	var writeTest bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check exiftool, the config, the NAS connections and the cards",
		Long: `doctor checks what camback needs and reports each item as OK, WARN or FAIL,
with what to do about it: exiftool, the config file, each remote (name lookup,
TCP connection, host key, login, SFTP), each camera's dest and sha256sum on
the NAS, and the memory cards that are inserted.

Without --write-test nothing is written to the NAS, and sha256sum is checked
against a file already in dest. As with "camback run", an unknown host key
is shown and added to known_hosts if you trust it.

--write-test writes a probe file into each camera's dest (or, if dest does
not exist yet, the nearest folder above it) and removes it again. It checks
that camback can write there, that sha256sum on the NAS hashes the same file
(shell_path_map), and that file_mode and dir_mode are applied. On a shared
folder with Windows ACLs, chmod may change the ACL of the probe.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.doctor(cmd.Context(), writeTest) },
	}
	cmd.Flags().BoolVar(&writeTest, "write-test", false, "write and remove a probe file in each camera's dest")
	return cmd
}

type status string

const (
	statusOK   status = "OK"
	statusSkip status = "SKIP"
	statusWarn status = "WARN"
	statusFail status = "FAIL"
)

type doctor struct {
	out          io.Writer
	warns, fails int
}

func (d *doctor) section(title string) { fmt.Fprintf(d.out, "\n%s\n", title) }

// report prints one item. fix, if any, says what to do about it.
func (d *doctor) report(st status, what, detail, fix string) {
	switch st {
	case statusWarn:
		d.warns++
	case statusFail:
		d.fails++
	}
	line := what
	if detail != "" {
		line += ": " + detail
	}
	lines := strings.Split(line, "\n")
	fmt.Fprintf(d.out, "  %-4s  %s\n", st, lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintf(d.out, "        %s\n", l)
	}
	if fix != "" {
		fmt.Fprintf(d.out, "        fix: %s\n", fix)
	}
}

func (a *app) doctor(ctx context.Context, writeTest bool) error {
	d := &doctor{out: a.stdout}

	d.section("exiftool")
	if v, p, err := meta.Version(ctx); err != nil {
		d.report(statusFail, "exiftool", err.Error(), "install it (macOS: brew install exiftool; Windows: rename exiftool(-k).exe to exiftool.exe) and put it on PATH")
	} else {
		d.report(statusOK, "exiftool", fmt.Sprintf("%s (%s)", v, p), "")
	}

	d.section("Config")
	cfg := d.config(a.configFlag)

	if cfg != nil {
		names := make([]string, 0, len(cfg.Remotes))
		for name := range cfg.Remotes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := a.doctorRemote(ctx, d, cfg, name, writeTest); err != nil {
				return err
			}
		}
	}

	d.section("Memory cards")
	d.cards(cfg)

	fmt.Fprintln(d.out)
	if d.fails > 0 {
		return &ExitError{Code: ExitFailure, Err: fmt.Errorf("%s and %s found", plural(d.fails, "problem"), plural(d.warns, "warning"))}
	}
	if d.warns > 0 {
		fmt.Fprintf(d.out, "No problems, %s.\n", plural(d.warns, "warning"))
	} else {
		fmt.Fprintln(d.out, "No problems found.")
	}
	return nil
}

// config loads the config file and reports where it came from; nil when it
// cannot be used.
func (d *doctor) config(flag string) *config.Config {
	p, from, err := config.ResolvePathFrom(flag)
	if err != nil {
		d.report(statusFail, "config file", err.Error(), "")
		return nil
	}
	cfg, err := config.Load(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		d.report(statusFail, "config file", fmt.Sprintf("%s (%s) does not exist", p, from),
			fmt.Sprintf("copy config.example.toml there and edit it, or point --config or $%s at your config file", config.EnvConfig))
		return nil
	case err != nil:
		d.report(statusFail, "config file", fmt.Sprintf("%s (%s) is not valid:\n%v", p, from, err), "fix the items above")
		return nil
	}
	d.report(statusOK, "config file", fmt.Sprintf("%s (%s); %s, %s", p, from,
		plural(len(cfg.Cameras), "camera"), plural(len(cfg.Remotes), "remote")), "")
	return cfg
}

// warnCollector keeps known_hosts warnings for the report instead of
// printing them as they come.
type warnCollector struct {
	sftp.Prompter
	warnings []string
}

func (w *warnCollector) Warn(msg string) { w.warnings = append(w.warnings, msg) }

// doctorRemote checks the connection to one remote, then the dest of each
// camera that uses it. Only a cancellation (Ctrl-C) is returned as an error.
func (a *app) doctorRemote(ctx context.Context, d *doctor, cfg *config.Config, name string, writeTest bool) error {
	r := cfg.Remotes[name]
	d.section(fmt.Sprintf("Remote %q (%s@%s:%d)", name, r.User, r.Host, r.Port))
	var cams []*config.Camera
	for i := range cfg.Cameras {
		if cfg.Cameras[i].Remote == name {
			cams = append(cams, &cfg.Cameras[i])
		}
	}

	p := &warnCollector{Prompter: a.prompt}
	tr := &sftp.Trace{}
	t, err := dialTraceFunc(ctx, r, p, tr)
	for _, w := range p.warnings {
		d.report(statusWarn, "known_hosts", w, "remove or fix that line")
	}
	d.connection(r, tr, err)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, cam := range cams {
			d.report(statusSkip, fmt.Sprintf("camera %q", cam.ID), "not checked: cannot connect", "")
		}
		return nil
	}
	defer t.Close()

	if len(cams) == 0 {
		d.report(statusWarn, "cameras", "no camera uses this remote", "remove it, or set remote = \""+name+"\" in a [[camera]]")
	}
	for _, cam := range cams {
		d.dest(ctx, t, name, r, cam, writeTest)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// connection reports each stage of connecting up to the one that failed.
func (d *doctor) connection(r config.Remote, tr *sftp.Trace, err error) {
	reached := func(s sftp.Stage) bool { return err == nil || s < tr.Failed }
	failed := func(s sftp.Stage) bool { return err != nil && s == tr.Failed }
	if err != nil && tr.Failed == 0 {
		d.report(statusFail, "connection", err.Error(), "")
		return
	}

	switch {
	case reached(sftp.StageResolve):
		d.report(statusOK, "name lookup", strings.Join(tr.Addrs, ", "), "")
	case failed(sftp.StageResolve):
		d.report(statusFail, "name lookup", err.Error(),
			"check host in the config; a .local name needs the NAS on the same network, or use its IP address")
		return
	}
	switch {
	case reached(sftp.StageConnect) && tr.Remote == "":
		// Failed reading known_hosts or a key file, before connecting.
	case reached(sftp.StageConnect):
		d.report(statusOK, "TCP connection", tr.Remote, "")
	case failed(sftp.StageConnect):
		d.report(statusFail, "TCP connection", err.Error(),
			fmt.Sprintf("check that the NAS is on and that %d is its SFTP port; a firewall may block it", r.Port))
		return
	}
	if failed(sftp.StageHandshake) {
		d.report(statusFail, "SSH handshake", err.Error(), "check that the port belongs to an SSH/SFTP server")
		return
	}
	switch {
	case reached(sftp.StageHostKey) && tr.HostKey == nil:
		// Failed reading a key file, before connecting.
	case reached(sftp.StageHostKey):
		where := "in known_hosts"
		if tr.HostKeyAdded {
			where = "added to known_hosts now"
		}
		d.report(statusOK, "host key", fmt.Sprintf("%s %s (%s)", tr.HostKey.Type(), ssh.FingerprintSHA256(tr.HostKey), where), "")
	case failed(sftp.StageHostKey):
		fix := ""
		if errors.Is(err, errNotInteractive) {
			fix = "run camback doctor in a terminal to see the host key and trust it"
		}
		d.report(statusFail, "host key", err.Error(), fix)
		return
	}
	switch {
	case reached(sftp.StageAuth):
		if tr.Auth == "password" || strings.HasPrefix(tr.Auth, "password ") {
			d.report(statusOK, "login", tr.Auth+" (set up an SSH key to log in without it; see the README)", "")
		} else {
			d.report(statusOK, "login", tr.Auth, "")
		}
	case failed(sftp.StageAuth):
		d.report(statusFail, "login", err.Error(),
			fmt.Sprintf("check user = %q, the password, or that the NAS has your public key in ~/.ssh/authorized_keys (see the README)", r.User))
		return
	}
	switch {
	case reached(sftp.StageSFTP):
		d.report(statusOK, "SFTP", "", "")
	case failed(sftp.StageSFTP):
		d.report(statusFail, "SFTP", err.Error(), "enable SFTP on the NAS (Synology: Control Panel > File Services > FTP > SFTP)")
	}
}

// dest checks one camera's destination folder and sha256sum there.
func (d *doctor) dest(ctx context.Context, t transport.Transport, remoteName string, r config.Remote, cam *config.Camera, writeTest bool) {
	d.section(fmt.Sprintf("Camera %q (%s:%s)", cam.ID, remoteName, cam.Dest))
	const what = "dest"
	dest := transport.Path(cam.Dest)
	shellNote := ""
	if m := sftp.MapPath(r.ShellPathMap, cam.Dest); m != cam.Dest {
		shellNote = fmt.Sprintf(" (%s in the NAS shell)", m)
	}

	dir := dest // where a probe can go
	exists := false
	fi, err := t.Stat(dest)
	switch {
	case err == nil && fi.IsDir:
		exists = true
		d.report(statusOK, what, "exists", "")
	case err == nil:
		d.report(statusFail, what, "is a file, not a folder", "change dest in the config")
		return
	case errors.Is(err, fs.ErrNotExist):
		dir, err = existingParent(t, dest)
		if err != nil {
			d.report(statusFail, what, err.Error(), "")
			return
		}
		d.report(statusWarn, what, "does not exist yet",
			"camback creates it on the first run; if this camera has been backed up before, check dest")
	default:
		d.report(statusFail, what, err.Error(), fmt.Sprintf("check that %s may read it", r.User))
		return
	}

	if !writeTest {
		d.remoteHashReadOnly(ctx, t, dest, exists, shellNote)
		d.report(statusSkip, "write access", "not checked; use --write-test", "")
		return
	}
	d.writeTest(t, r, dir, shellNote)
}

// remoteHashReadOnly compares sha256sum on the NAS with a download of a
// file already in dest.
func (d *doctor) remoteHashReadOnly(ctx context.Context, t transport.Transport, dest transport.Path, exists bool, shellNote string) {
	if !exists {
		d.report(statusSkip, "sha256sum on the NAS", "no file to compare; use --write-test", "")
		return
	}
	sample, err := sampleFile(t, dest)
	if err != nil {
		d.report(statusWarn, "sha256sum on the NAS", fmt.Sprintf("cannot list %s: %v", dest, err), "")
		return
	}
	if sample == "" {
		d.report(statusSkip, "sha256sum on the NAS", "no file in dest to compare; use --write-test", "")
		return
	}
	sum, err := hashing.ReadBack(ctx, t, sample)
	if err != nil {
		d.report(statusWarn, "sha256sum on the NAS", fmt.Sprintf("cannot download %s: %v", sample, err), "")
		return
	}
	d.remoteHash(hashing.CheckRemote(t, sample, sum), "matches a download of "+sample.String(), shellNote)
}

// remoteHash reports the result of hashing.CheckRemote.
func (d *doctor) remoteHash(err error, okDetail, shellNote string) {
	const what = "sha256sum on the NAS"
	fallback := "otherwise camback compares and verifies files by downloading them (slower)"
	switch {
	case err == nil:
		d.report(statusOK, what, okDetail, "")
	case errors.Is(err, hashing.ErrRemoteMismatch):
		d.report(statusWarn, what, "hashed a different file"+shellNote,
			"check shell_path_map: it must turn the SFTP path into the path the NAS shell sees; "+fallback)
	default:
		d.report(statusWarn, what, err.Error()+shellNote,
			"check shell_path_map and that sha256sum runs over SSH on the NAS; "+fallback)
	}
}

// writeTest writes a probe file in dir, checks sha256sum and the modes on
// it, and removes it.
func (d *doctor) writeTest(t transport.Transport, r config.Remote, dir transport.Path, shellNote string) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		d.report(statusFail, "write access", err.Error(), "")
		return
	}
	probe := dir.Join(".camback-probe-" + hex.EncodeToString(buf[:8]))
	w, err := t.Create(probe)
	if err == nil {
		_, err = w.Write(buf)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		t.Remove(probe)
		d.report(statusFail, "write access", fmt.Sprintf("cannot write in %s: %v", dir, err),
			fmt.Sprintf("give %s write permission on that shared folder", r.User))
		return
	}
	d.report(statusOK, "write access", "wrote and removed a probe file in "+dir.String(), "")

	sum := sha256.Sum256(buf)
	d.remoteHash(hashing.CheckRemote(t, probe, hex.EncodeToString(sum[:])), "matches the probe file", shellNote)

	if r.FileMode != nil {
		if err := execute.SetMode(t, probe, *r.FileMode); err != nil {
			d.report(statusFail, "file_mode", err.Error(), "")
		} else {
			d.report(statusOK, "file_mode", fmt.Sprintf("0o%03o is applied", uint32(*r.FileMode)), "")
		}
	}
	if err := t.Remove(probe); err != nil {
		d.report(statusWarn, "probe file", fmt.Sprintf("cannot remove %s: %v", probe, err), "delete it by hand")
	}

	if r.DirMode != nil {
		pdir := dir.Join(".camback-probe-" + hex.EncodeToString(buf[8:16]))
		if err := t.Mkdir(pdir); err != nil {
			d.report(statusFail, "dir_mode", fmt.Sprintf("cannot create a folder in %s: %v", dir, err), "")
			return
		}
		if err := execute.SetMode(t, pdir, *r.DirMode); err != nil {
			d.report(statusFail, "dir_mode", err.Error(), "")
		} else {
			d.report(statusOK, "dir_mode", fmt.Sprintf("0o%03o is applied", uint32(*r.DirMode)), "")
		}
		if err := t.Remove(pdir); err != nil {
			d.report(statusWarn, "probe folder", fmt.Sprintf("cannot remove %s: %v", pdir, err), "delete it by hand")
		}
	}
}

// existingParent returns the nearest folder above p that exists.
func existingParent(t transport.Transport, p transport.Path) (transport.Path, error) {
	for q := p.Dir(); ; q = q.Dir() {
		fi, err := t.Stat(q)
		switch {
		case err == nil && fi.IsDir:
			return q, nil
		case err == nil:
			return "", fmt.Errorf("%s is a file, not a folder", q)
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		}
		if q.Dir() == q {
			return "", fmt.Errorf("no folder above it exists")
		}
	}
}

// sampleMaxDirs bounds how many folders sampleFile lists.
const sampleMaxDirs = 20

// sampleFile returns the smallest non-empty file found in the first folders
// under dir (breadth first), or "" if there is none. Hidden files, such as
// leftovers of an interrupted run, are ignored.
func sampleFile(t transport.Transport, dir transport.Path) (transport.Path, error) {
	queue := []transport.Path{dir}
	var best transport.Path
	var bestSize int64
	for n := 0; len(queue) > 0 && n < sampleMaxDirs; n++ {
		d := queue[0]
		queue = queue[1:]
		list, err := t.ReadDir(d)
		if err != nil {
			if d == dir {
				return "", err
			}
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		for _, fi := range list {
			switch {
			case strings.HasPrefix(fi.Name, "."):
			case fi.IsDir:
				queue = append(queue, d.Join(fi.Name))
			case fi.Size > 0 && (best == "" || fi.Size < bestSize):
				best, bestSize = d.Join(fi.Name), fi.Size
			}
		}
	}
	return best, nil
}

// cards reports the mounted memory cards and the camera their volume label
// selects.
func (d *doctor) cards(cfg *config.Config) {
	cands := source.Detect()
	if len(cands) == 0 {
		d.report(statusSkip, "memory card", "none with a DCIM folder is mounted", "")
		return
	}
	for _, c := range cands {
		detail := fmt.Sprintf("%s (volume %q)", c.Root, c.Label)
		if cfg != nil {
			var ids []string
			for _, cam := range cfg.Cameras {
				if cam.VolumeLabel != "" && strings.EqualFold(cam.VolumeLabel, c.Label) {
					ids = append(ids, cam.ID)
				}
			}
			if len(ids) > 0 {
				detail += "; volume_label of " + strings.Join(ids, ", ")
			}
		}
		d.report(statusOK, "memory card", detail, "")
	}
}
