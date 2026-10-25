//go:build e2e

package e2e

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/TatsuyaOGth/camback/internal/meta"
)

const image = "camback-e2e-sshd"

// server is the OpenSSH container shared by all tests. Its /volume1/photo is
// bind-mounted from photoDir, so tests check the NAS side on the local disk.
var server struct {
	host, port string
	keyPath    string // client private key (OpenSSH format)
	knownHosts string // line for the container's host key
	photoDir   string // = /photo over SFTP, /volume1/photo in the shell
	signer     ssh.Signer
}

// bin is the camback binary under test.
var bin string

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if runtime.GOOS != "linux" {
		fmt.Println("e2e: skipped, Linux only")
		return 0
	}
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Println("e2e: docker is required:", err)
		return 1
	}
	if err := meta.Check(); err != nil {
		fmt.Println("e2e: exiftool is required:", err)
		return 1
	}
	work, err := os.MkdirTemp("", "camback-e2e-")
	if err != nil {
		fmt.Println("e2e:", err)
		return 1
	}
	defer os.RemoveAll(work)

	stop, err := setup(work)
	if stop != nil {
		defer stop()
	}
	if err != nil {
		fmt.Println("e2e: setup:", err)
		return 1
	}
	return m.Run()
}

func setup(work string) (stop func(), err error) {
	bin = filepath.Join(work, "camback")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/TatsuyaOGth/camback/cmd/camback").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build camback: %v\n%s", err, out)
	}

	// The container user gets our uid, so we can modify and remove what it
	// writes. Root keeps the image default (root can remove anything).
	uid := os.Getuid()
	if uid == 0 {
		uid = 1000
	}
	args := []string{"build", "-q", "-t", image, "--build-arg", "NAS_UID=" + strconv.Itoa(uid)}
	if os.Getenv("HTTPS_PROXY") != "" || os.Getenv("HTTP_PROXY") != "" {
		// A proxy on localhost is only reachable from the host network.
		args = append(args, "--network", "host", "--build-arg", "HTTP_PROXY", "--build-arg", "HTTPS_PROXY")
	}
	if out, err := exec.Command("docker", append(args, "sshd")...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker build: %v\n%s", err, out)
	}

	server.photoDir = filepath.Join(work, "photo")
	if err := os.Mkdir(server.photoDir, 0o777); err != nil {
		return nil, err
	}
	if err := os.Chmod(server.photoDir, 0o777); err != nil { // umask
		return nil, err
	}
	out, err := exec.Command("docker", "run", "-d", "--rm", "-p", "127.0.0.1::22",
		"-v", server.photoDir+":/volume1/photo", image).Output()
	if err != nil {
		return nil, fmt.Errorf("docker run: %v", stderrOf(err))
	}
	id := strings.TrimSpace(string(out))
	stop = func() { exec.Command("docker", "rm", "-f", id).Run() }

	out, err = exec.Command("docker", "port", id, "22/tcp").Output()
	if err != nil {
		return stop, fmt.Errorf("docker port: %v", stderrOf(err))
	}
	hp := strings.Fields(string(out))[0] // first mapping, e.g. 127.0.0.1:32768
	if server.host, server.port, err = net.SplitHostPort(hp); err != nil {
		return stop, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return stop, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return stop, err
	}
	server.keyPath = filepath.Join(work, "id_ed25519")
	if err := os.WriteFile(server.keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		return stop, err
	}
	if server.signer, err = ssh.NewSignerFromKey(priv); err != nil {
		return stop, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return stop, err
	}
	install := exec.Command("docker", "exec", "-i", "-u", "nas", id, "sh", "-c",
		"umask 077 && cat > /home/nas/.ssh/authorized_keys")
	install.Stdin = bytes.NewReader(ssh.MarshalAuthorizedKey(sshPub))
	if out, err := install.CombinedOutput(); err != nil {
		return stop, fmt.Errorf("install key: %v\n%s", err, out)
	}

	hostKey, err := exec.Command("docker", "exec", id, "cat", "/etc/ssh/ssh_host_ed25519_key.pub").Output()
	if err != nil {
		return stop, fmt.Errorf("read host key: %v", stderrOf(err))
	}
	f := strings.Fields(string(hostKey))
	server.knownHosts = fmt.Sprintf("[%s]:%s %s %s\n", server.host, server.port, f[0], f[1])

	return stop, waitForSSH()
}

// waitForSSH waits until sshd accepts our key and starts the sftp subsystem.
func waitForSSH() error {
	var err error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		var c *ssh.Client
		if c, err = dialSSH(); err != nil {
			continue
		}
		var s *ssh.Session
		if s, err = c.NewSession(); err == nil {
			err = s.RequestSubsystem("sftp")
			s.Close()
		}
		c.Close()
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("sshd not ready: %w", err)
}

func dialSSH() (*ssh.Client, error) {
	return ssh.Dial("tcp", net.JoinHostPort(server.host, server.port), &ssh.ClientConfig{
		User:            "nas",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(server.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // only used by the tests themselves
		Timeout:         5 * time.Second,
	})
}

func stderrOf(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("%v: %s", err, ee.Stderr)
	}
	return err.Error()
}
