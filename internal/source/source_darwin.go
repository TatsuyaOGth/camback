package source

func candidateRoots() []string {
	return listDirs("/Volumes")
}

// On macOS the mount point name under /Volumes is the volume label.
func volumeLabel(root string) string { return "" }
