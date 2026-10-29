//go:build !windows

package sftp

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/TatsuyaOGth/camback/internal/config"
)

// startAgent serves keys from an in-process ssh-agent and points
// SSH_AUTH_SOCK at it. Call it after isolateHome, which clears SSH_AUTH_SOCK.
func startAgent(t *testing.T, keys ...any) {
	t.Helper()
	// A short path: Unix socket paths are limited to about 100 bytes, and
	// t.TempDir can be longer on macOS.
	dir, err := os.MkdirTemp("", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	kr := agent.NewKeyring()
	for _, k := range keys {
		if err := kr.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			t.Fatal(err)
		}
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { agent.ServeAgent(kr, conn); conn.Close() }()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)
}

// An RSA key in the agent must still sign with SHA-2 when doctor wraps it
// to see whether it logged in: OpenSSH 8.8 and later refuse "ssh-rsa"
// (SHA-1) signatures, as this server does.
func TestTraceAgentRSAKeySignsWithSHA2(t *testing.T) {
	isolateHome(t)
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	startAgent(t, priv)
	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	port := startServerWith(t, pub, "", func(c *ssh.ServerConfig) {
		c.PublicKeyAuthAlgorithms = []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512}
	})
	r := config.Remote{Host: "127.0.0.1", Port: port, User: "u"}

	// The password is never asked: the agent key logs in.
	if err := dialTest(t, port, "", &scriptedPrompter{}); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	tr, err := traceTest(t, r, &scriptedPrompter{})
	if err != nil {
		t.Fatalf("DialTrace: %v", err)
	}
	if want := "ssh-agent key (ssh-rsa " + ssh.FingerprintSHA256(pub) + ")"; tr.Auth != want {
		t.Errorf("auth = %q, want %q", tr.Auth, want)
	}
}

func TestTraceAgentKeyOverKeyFile(t *testing.T) {
	home := isolateHome(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	startAgent(t, priv)
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	// The same key, encrypted, in a key file: the agent's copy is used, so
	// no passphrase is asked.
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(home, "id_same")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	port := startServer(t, pub, "")

	p := &scriptedPrompter{}
	tr, err := traceTest(t, config.Remote{Host: "127.0.0.1", Port: port, User: "u", IdentityFile: keyPath}, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tr.Auth, "ssh-agent key (ssh-ed25519 ") || len(p.prompts) != 0 {
		t.Errorf("auth = %q, prompts = %q", tr.Auth, p.prompts)
	}
}
