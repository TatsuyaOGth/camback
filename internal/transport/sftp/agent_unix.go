//go:build !windows

package sftp

import (
	"errors"
	"net"
	"os"
)

// dialAgent connects to the ssh-agent named by SSH_AUTH_SOCK.
func dialAgent() (net.Conn, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, errors.New("SSH_AUTH_SOCK not set")
	}
	return net.Dial("unix", sock)
}
