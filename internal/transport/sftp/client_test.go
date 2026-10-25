package sftp

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	psftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/TatsuyaOGth/camback/internal/transport"
)

// newTestClient starts pkg/sftp's in-process server over in-memory pipes.
// The server exposes the real local file system, so remote paths equal
// local paths. Skipped on Windows, where the test server maps paths
// differently from a real NAS.
func newTestClient(t *testing.T, exec func(string) ([]byte, error)) (*Client, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("in-process SFTP server path mapping differs on Windows")
	}
	root := t.TempDir()
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	srv, err := psftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{serverR, serverW})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	fs, err := psftp.NewClientPipe(clientR, clientW, psftp.UseConcurrentWrites(true))
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(fs, exec, nil)
	t.Cleanup(func() { srv.Close(); c.Close() }) // server first, so the client sees EOF
	return c, filepath.ToSlash(root)
}

func TestRoundTrip(t *testing.T) {
	c, root := newTestClient(t, nil)
	dir := transport.Path(root).Join("photo", "GR3x", "DCIM")
	if err := c.MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	p := dir.Join("R0001.JPG")
	w, err := c.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	data := strings.Repeat("x", 1<<20) // large enough for concurrent writes
	if _, err := io.Copy(w, strings.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 10, 16, 9, 26, 29, 0, time.Local)
	if err := c.Chtimes(p, mtime); err != nil {
		t.Fatal(err)
	}
	fi, err := c.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size != int64(len(data)) || !fi.ModTime.Equal(mtime) {
		t.Errorf("stat = %+v", fi)
	}
	list, err := c.ReadDir(dir)
	if err != nil || len(list) != 1 || list[0].Name != "R0001.JPG" {
		t.Errorf("readdir = %+v, %v", list, err)
	}
	if _, err := c.ReadDir(dir.Join("missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("readdir missing: %v", err)
	}
	if _, err := c.Stat(dir.Join("missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat missing: %v", err)
	}
}

func TestRenameNoClobber(t *testing.T) {
	c, root := newTestClient(t, nil)
	a, b := transport.Path(root).Join("a"), transport.Path(root).Join("b")
	os.WriteFile(filepath.FromSlash(string(a)), []byte("A"), 0o644)
	os.WriteFile(filepath.FromSlash(string(b)), []byte("B"), 0o644)

	if err := c.Rename(a, b); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("rename onto existing: err = %v, want ErrExist", err)
	}
	if got, _ := os.ReadFile(filepath.FromSlash(string(b))); string(got) != "B" {
		t.Errorf("b was overwritten: %q", got)
	}
	cpath := transport.Path(root).Join("c")
	if err := c.Rename(a, cpath); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.FromSlash(string(cpath))); string(got) != "A" {
		t.Errorf("c = %q", got)
	}
}

func TestSHA256ViaShell(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum not available")
	}
	sh := func(cmd string) ([]byte, error) { return exec.Command("sh", "-c", cmd).Output() }
	c, root := newTestClient(t, sh)
	// A name with a quote, spaces and non-ASCII characters exercises ShellQuote.
	p := transport.Path(root).Join("it's a 写真 $(x).JPG")
	content := []byte("hello")
	if err := os.WriteFile(filepath.FromSlash(string(p)), content, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := c.SHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("got %s want %s", got, want)
	}
	if _, err := c.SHA256(transport.Path(root).Join("missing")); err == nil {
		t.Error("missing file should fail")
	}
}

func TestSHA256Unsupported(t *testing.T) {
	c := newClient(nil, nil, nil)
	if _, err := c.SHA256("/x"); !errors.Is(err, transport.ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
}

func TestMapPath(t *testing.T) {
	m := map[string]string{
		"/photo":       "/volume1/photo",
		"/home":        "/volume1/homes/user",
		"/home/shared": "/volume2/shared",
	}
	cases := map[string]string{
		"/photo/GR3x/DCIM/a.JPG": "/volume1/photo/GR3x/DCIM/a.JPG",
		"/photo":                 "/volume1/photo",
		"/photos/a.JPG":          "/photos/a.JPG", // element-wise prefix only
		"/home/backups/x.mp4":    "/volume1/homes/user/backups/x.mp4",
		"/home/shared/x":         "/volume2/shared/x", // longest prefix wins
		"/other/x":               "/other/x",
	}
	for in, want := range cases {
		if got := MapPath(m, in); got != want {
			t.Errorf("MapPath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := MapPath(map[string]string{"/": "/volume1"}, "/a/b"); got != "/volume1/a/b" {
		t.Errorf("root map: %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	if got := ShellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("got %s", got)
	}
}

type fakePrompter struct {
	confirm  bool
	confirms int
	warnings []string
}

func (f *fakePrompter) Password(string) (string, error) {
	return "", errors.New("no password in tests")
}
func (f *fakePrompter) Confirm(string) (bool, error) { f.confirms++; return f.confirm, nil }
func (f *fakePrompter) Warn(msg string)              { f.warnings = append(f.warnings, msg) }

func newKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestHostKeyTOFU(t *testing.T) {
	kh := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(kh, nil, 0o600)
	addr := "nas.local:2222"
	remote := &net.TCPAddr{IP: net.IPv4(192, 168, 1, 10), Port: 2222}
	key := newKey(t)

	// Unknown host, user declines.
	p := &fakePrompter{confirm: false}
	cb, algos, err := newHostKeyCallback(kh, addr, p)
	if err != nil {
		t.Fatal(err)
	}
	if algos != nil {
		t.Errorf("unknown host should not restrict algorithms: %v", algos)
	}
	if err := cb(addr, remote, key); err == nil {
		t.Fatal("declined host must fail")
	}

	// Unknown host, user accepts: line is appended.
	p.confirm = true
	if err := cb(addr, remote, key); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(kh)
	if !strings.HasPrefix(string(data), "[nas.local]:2222 ssh-ed25519 ") {
		t.Errorf("known_hosts = %q", data)
	}

	// Reload: known key passes without prompting, other key is a mismatch.
	p2 := &fakePrompter{}
	cb2, algos2, err := newHostKeyCallback(kh, addr, p2)
	if err != nil {
		t.Fatal(err)
	}
	if len(algos2) != 1 || algos2[0] != ssh.KeyAlgoED25519 {
		t.Errorf("algos = %v", algos2)
	}
	if err := cb2(addr, remote, key); err != nil {
		t.Errorf("known key: %v", err)
	}
	if err := cb2(addr, remote, newKey(t)); err == nil || !strings.Contains(err.Error(), "MISMATCH") {
		t.Errorf("changed key: err = %v", err)
	}
	if p2.confirms != 0 {
		t.Errorf("should not prompt for known or mismatched hosts")
	}
}

func TestHostKeySkipsUnparsableLines(t *testing.T) {
	kh := filepath.Join(t.TempDir(), "known_hosts")
	addr := "nas.local:2222"
	remote := &net.TCPAddr{IP: net.IPv4(192, 168, 1, 10), Port: 2222}
	key := newKey(t)
	futureKey := ssh.Marshal(struct {
		Name string
		Blob []byte
	}{"ssh-future-key", []byte{1, 2, 3}})
	lines := []string{
		"# comment",
		"bad.example ssh-ed25519 !!!not-base64\r", // CRLF line
		knownhosts.Line([]string{"[nas.local]:2222"}, key),
		"@unknown-marker host.example ssh-ed25519 AAAA",
		"new.example ssh-future-key " + base64.StdEncoding.EncodeToString(futureKey),
		strings.Repeat("x", maxKnownHostsLine+1),
	}
	orig := strings.Join(lines, "\n") + "\n"
	os.WriteFile(kh, []byte(orig), 0o600)

	p := &fakePrompter{}
	cb, algos, err := newHostKeyCallback(kh, addr, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(algos) != 1 || algos[0] != ssh.KeyAlgoED25519 {
		t.Errorf("algos = %v", algos)
	}
	if err := cb(addr, remote, key); err != nil {
		t.Errorf("known key: %v", err)
	}
	// The mismatch names the real file and the line number in it.
	err = cb(addr, remote, newKey(t))
	if err == nil || !strings.Contains(err.Error(), kh+" line 3") {
		t.Errorf("changed key: err = %v", err)
	}
	if p.confirms != 0 {
		t.Errorf("should not prompt for known or mismatched hosts")
	}
	var got []string
	for _, w := range p.warnings {
		for _, n := range []string{"2", "4", "5", "6"} {
			if strings.Contains(w, kh+" line "+n+" ") {
				got = append(got, n)
			}
		}
	}
	if strings.Join(got, ",") != "2,4,5,6" {
		t.Errorf("warnings = %q", p.warnings)
	}

	// A new host is appended to the real file; the skipped lines are kept.
	p.confirm = true
	if err := cb("other.local:22", remote, key); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(kh)
	if !strings.HasPrefix(string(data), orig) || !strings.Contains(string(data)[len(orig):], "other.local ssh-ed25519 ") {
		t.Errorf("known_hosts = %q", data)
	}
}

func TestHostKeyUnparsableRevokedLineFails(t *testing.T) {
	kh := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(kh, []byte("@revoked * ssh-ed25519 !!!not-base64\n"), 0o600)
	if _, _, err := newHostKeyCallback(kh, "nas.local:22", &fakePrompter{}); err == nil || !strings.Contains(err.Error(), "@revoked") {
		t.Errorf("err = %v", err)
	}
}

func TestDialWarnsOnce(t *testing.T) {
	p := &fakePrompter{}
	w := &onceWarner{Prompter: p, seen: map[string]bool{}}
	w.Warn("a")
	w.Warn("a")
	w.Warn("b")
	if strings.Join(p.warnings, ",") != "a,b" {
		t.Errorf("warnings = %q", p.warnings)
	}
}

// The accepted key must go on a line of its own and be remembered, even when
// the last line has no newline (a skipped line or a valid one).
func TestHostKeyAppendAfterUnterminatedLine(t *testing.T) {
	addr := "nas.local:2222"
	remote := &net.TCPAddr{IP: net.IPv4(192, 168, 1, 10), Port: 2222}
	for name, last := range map[string]string{
		"skipped": "bad.example ssh-ed25519 !!!not-base64",
		"valid":   knownhosts.Line([]string{"other.example"}, newKey(t)),
	} {
		t.Run(name, func(t *testing.T) {
			kh := filepath.Join(t.TempDir(), "known_hosts")
			os.WriteFile(kh, []byte(last), 0o600)
			key := newKey(t)
			cb, _, err := newHostKeyCallback(kh, addr, &fakePrompter{confirm: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := cb(addr, remote, key); err != nil {
				t.Fatal(err)
			}
			if data, _ := os.ReadFile(kh); !strings.HasPrefix(string(data), last+"\n[nas.local]:2222 ") {
				t.Errorf("known_hosts = %q", data)
			}

			p := &fakePrompter{}
			cb2, _, err := newHostKeyCallback(kh, addr, p)
			if err != nil {
				t.Fatal(err)
			}
			if err := cb2(addr, remote, key); err != nil {
				t.Errorf("accepted key not remembered: %v", err)
			}
			if err := cb2(addr, remote, newKey(t)); err == nil || !strings.Contains(err.Error(), "MISMATCH") {
				t.Errorf("changed key: err = %v", err)
			}
			if p.confirms != 0 {
				t.Errorf("should not prompt after reload")
			}
		})
	}
}

// A line the knownhosts scanner can still read is kept, however long.
func TestHostKeyKeepsLongReadableLine(t *testing.T) {
	kh := filepath.Join(t.TempDir(), "known_hosts")
	addr := "nas.local:2222"
	remote := &net.TCPAddr{IP: net.IPv4(192, 168, 1, 10), Port: 2222}
	key := newKey(t)
	line := knownhosts.Line([]string{"[nas.local]:2222"}, key) + " "
	line += strings.Repeat("c", maxKnownHostsLine-len(line)) // long trailing comment
	os.WriteFile(kh, []byte(line+"\n"), 0o600)

	p := &fakePrompter{}
	cb, _, err := newHostKeyCallback(kh, addr, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.warnings) != 0 {
		t.Errorf("warnings = %q", p.warnings)
	}
	if err := cb(addr, remote, newKey(t)); err == nil || !strings.Contains(err.Error(), "MISMATCH") {
		t.Errorf("changed key: err = %v", err)
	}
}
