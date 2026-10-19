package source

import (
	"os"
	"os/user"
	"path/filepath"
)

func candidateRoots() []string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	var out []string
	for _, dir := range []string{
		filepath.Join("/media", name),
		filepath.Join("/run/media", name),
		"/media",
	} {
		out = append(out, listDirs(dir)...)
	}
	return out
}

// udisks names the mount point after the volume label.
func volumeLabel(root string) string { return "" }
