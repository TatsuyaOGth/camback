package sftp

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	psftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/TatsuyaOGth/camback/internal/config"
)

// scriptedPrompter answers Password prompts from a queue and records them.
type scriptedPrompter struct {
	answers  []string
	prompts  []string
	warnings []string
}

func (s *scriptedPrompter) Password(prompt string) (string, error) {
	s.prompts = append(s.prompts, prompt)
	if len(s.answers) == 0 {
		return "", errors.New("no more answers")
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a, nil
}

func (s *scriptedPrompter) Confirm(string) (bool, error) { return true, nil } // trust the test host key
func (s *scriptedPrompter) Warn(msg string)              { s.warnings = append(s.warnings, msg) }

func (s *scriptedPrompter) asked(substr string) int {
	n := 0
	for _, p := range s.prompts {
		if strings.Contains(p, substr) {
			n++
		}
	}
	return n
}

// startServer runs an in-process SSH server with an SFTP subsystem.
// acceptKey (may be nil) is accepted for public key auth, password for
// password auth.
func startServer(t *testing.T, acceptKey ssh.PublicKey, password string) int {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if acceptKey != nil && bytes.Equal(k.Marshal(), acceptKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("key not authorized")
		},
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == password {
				return nil, nil
			}
			return nil, errors.New("wrong password")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, cfg)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func serveConn(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range creqs {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				req.Reply(ok, nil)
				if ok {
					srv, err := psftp.NewServer(ch)
					if err == nil {
						go func() { srv.Serve(); ch.Close() }()
					}
				}
			}
		}()
	}
}

// encryptedKey writes a passphrase-protected key and returns its path and
// public key.
func encryptedKey(t *testing.T, dir, passphrase string) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte(passphrase))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "id_test")
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	return p, sshPub
}

func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	return home
}

func dialTest(t *testing.T, port int, keyPath string, p Prompter) error {
	t.Helper()
	c, err := Dial(t.Context(), config.Remote{Host: "127.0.0.1", Port: port, User: "u", IdentityFile: keyPath}, p)
	if err == nil {
		c.Close()
	}
	return err
}

func TestDialEncryptedKeyAccepted(t *testing.T) {
	home := isolateHome(t)
	keyPath, pub := encryptedKey(t, home, "s3cret")
	port := startServer(t, pub, "")

	p := &scriptedPrompter{answers: []string{"wrong", "s3cret"}}
	if err := dialTest(t, port, keyPath, p); err != nil {
		t.Fatal(err)
	}
	if p.asked("passphrase") != 2 || p.asked("password") != 0 {
		t.Errorf("prompts = %q", p.prompts)
	}
	if data, _ := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts")); !bytes.Contains(data, []byte("[127.0.0.1]:")) {
		t.Errorf("host key not recorded: %q", data)
	}
}

func TestDialEncryptedKeyNotAcceptedIsNeverUnlocked(t *testing.T) {
	home := isolateHome(t)
	keyPath, _ := encryptedKey(t, home, "s3cret")
	port := startServer(t, nil, "pw") // server only knows the password

	p := &scriptedPrompter{answers: []string{"pw"}}
	if err := dialTest(t, port, keyPath, p); err != nil {
		t.Fatal(err)
	}
	if p.asked("passphrase") != 0 || p.asked("password") != 1 {
		t.Errorf("the passphrase of a key the server does not accept must not be asked: %q", p.prompts)
	}
}

func TestDialSkippedKeyFallsBackToPassword(t *testing.T) {
	home := isolateHome(t)
	keyPath, pub := encryptedKey(t, home, "s3cret")
	port := startServer(t, pub, "pw")

	p := &scriptedPrompter{answers: []string{"", "pw"}} // empty passphrase = skip
	if err := dialTest(t, port, keyPath, p); err != nil {
		t.Fatal(err)
	}
	if p.asked("passphrase") != 1 || p.asked("password") != 1 {
		t.Errorf("prompts = %q", p.prompts)
	}
}

func TestDialSkipsUnparsableKnownHostsLine(t *testing.T) {
	home := isolateHome(t)
	keyPath, pub := encryptedKey(t, home, "s3cret")
	port := startServer(t, pub, "pw")
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), []byte("bad.example ssh-ed25519 !!!\n"), 0o600)

	// Skipping the key makes Dial connect twice; the warning is shown once.
	p := &scriptedPrompter{answers: []string{"", "pw"}}
	if err := dialTest(t, port, keyPath, p); err != nil {
		t.Fatal(err)
	}
	if len(p.warnings) != 1 || !strings.Contains(p.warnings[0], "line 1 is ignored") {
		t.Errorf("warnings = %q", p.warnings)
	}
}
