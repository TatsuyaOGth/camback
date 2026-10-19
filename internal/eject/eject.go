// Package eject safely removes a memory card using each OS's own tool.
package eject

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Eject unmounts (and where possible powers off) the volume at root.
func Eject(root string) error {
	switch runtime.GOOS {
	case "darwin":
		return run("diskutil", "eject", root)
	case "windows":
		drive := filepath.VolumeName(root) // "E:"
		if drive == "" {
			return fmt.Errorf("cannot find the drive letter of %s", root)
		}
		// Shell.Application's "Eject" verb is what Explorer uses. It runs
		// asynchronously and never reports failure, so success is decided by
		// waiting for the drive to disappear.
		script := fmt.Sprintf("(New-Object -ComObject Shell.Application).Namespace(17).ParseName('%s').InvokeVerb('Eject')", drive)
		if err := run("powershell", "-NoProfile", "-NonInteractive", "-Command", script); err != nil {
			return err
		}
		return waitGone(drive+`\`, windowsEjectTimeout, 250*time.Millisecond, statExists)
	case "linux":
		out, err := exec.Command("findmnt", "-n", "-o", "SOURCE", "--target", root).Output()
		if err != nil {
			return fmt.Errorf("findmnt %s: %w", root, err)
		}
		dev := strings.TrimSpace(string(out))
		if err := run("udisksctl", "unmount", "-b", dev); err != nil {
			return err
		}
		return run("udisksctl", "power-off", "-b", dev)
	default:
		return fmt.Errorf("eject is not supported on %s", runtime.GOOS)
	}
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

const windowsEjectTimeout = 10 * time.Second

func statExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// waitGone polls until root no longer exists (ejected media make the drive
// disappear or report "not ready"). It fails if root is still there after
// timeout, e.g. because a program keeps a file on the card open.
func waitGone(root string, timeout, poll time.Duration, exists func(string) bool) error {
	deadline := time.Now().Add(timeout)
	for {
		if !exists(root) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is still mounted after %s; close any program using the card and eject it from Explorer before removing it", root, timeout)
		}
		time.Sleep(poll)
	}
}
