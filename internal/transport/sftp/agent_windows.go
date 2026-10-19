package sftp

import (
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// dialAgent connects to the Windows OpenSSH Agent service (named pipe).
func dialAgent() (net.Conn, error) {
	timeout := 2 * time.Second
	return winio.DialPipe(`\\.\pipe\openssh-ssh-agent`, &timeout)
}
