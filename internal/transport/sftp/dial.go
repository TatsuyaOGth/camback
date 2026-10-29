package sftp

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	psftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/TatsuyaOGth/camback/internal/config"
)

// Prompter asks the user for secrets and confirmations on the terminal.
type Prompter interface {
	Password(prompt string) (string, error)
	Confirm(prompt string) (bool, error)
	Warn(msg string)
}

// onceWarner shows each warning once, although Dial may connect again.
type onceWarner struct {
	Prompter
	seen map[string]bool
}

func (w *onceWarner) Warn(msg string) {
	if !w.seen[msg] {
		w.seen[msg] = true
		w.Prompter.Warn(msg)
	}
}

const dialTimeout = 15 * time.Second

// Dial connects to the remote and opens an SFTP session.
//
// Authentication order: ssh-agent, key files, password (asked once, never
// stored). Host keys are checked against ~/.ssh/known_hosts; an unknown host
// is shown to the user and added on confirmation, a changed key aborts.
// Lines of known_hosts that cannot be parsed are skipped with a warning.
func Dial(ctx context.Context, r config.Remote, p Prompter) (*Client, error) {
	return dial(ctx, r, p, nil)
}

func dial(ctx context.Context, r config.Remote, p Prompter, tr *Trace) (*Client, error) {
	addr := net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
	p = &onceWarner{Prompter: p, seen: map[string]bool{}}
	st := &authState{skipped: map[string]bool{}, unlocked: map[string]ssh.Signer{}}
	for {
		c, err := dialOnce(ctx, r, p, addr, st, tr)
		// Skipping a key the server accepted aborts the handshake (x/crypto
		// cannot move on from a key it is about to sign with), so connect
		// again without that key. Answers already given are reused.
		var skip *keySkippedError
		if errors.As(err, &skip) && !st.skipped[skip.path] {
			st.skipped[skip.path] = true
			continue
		}
		return c, err
	}
}

func dialOnce(ctx context.Context, r config.Remote, p Prompter, addr string, st *authState, tr *Trace) (*Client, error) {
	tr.start()
	if err := tr.resolve(ctx, r.Host); err != nil {
		return nil, err
	}
	hostKeyCB, algos, err := hostKeyCallback(addr, tr.prompter(p))
	if err != nil {
		return nil, tr.fail(StageHostKey, err)
	}
	hostKeyCB, hostKeyChecked := tr.hostKey(hostKeyCB)
	auth, closeAgent, err := authMethods(r, p, st, tr)
	if err != nil {
		return nil, tr.fail(StageAuth, err)
	}
	defer closeAgent()

	cfg := &ssh.ClientConfig{
		User:              r.User,
		Auth:              auth,
		HostKeyCallback:   hostKeyCB,
		HostKeyAlgorithms: algos,
		Timeout:           dialTimeout,
	}
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, tr.fail(StageConnect, fmt.Errorf("connect %s: %w", addr, err))
	}
	tr.connected(conn)
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, tr.handshakeFailed(hostKeyChecked, fmt.Errorf("ssh %s@%s: %w", r.User, addr, err))
	}
	sshClient := ssh.NewClient(c, chans, reqs)

	fs, err := psftp.NewClient(sshClient, psftp.UseConcurrentWrites(true))
	if err != nil {
		sshClient.Close()
		return nil, tr.fail(StageSFTP, fmt.Errorf("sftp %s: %w (is the SFTP service enabled on this port?)", addr, err))
	}
	exec := func(cmd string) ([]byte, error) {
		s, err := sshClient.NewSession()
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return s.Output(cmd)
	}
	cl := newClient(fs, exec, r.ShellPathMap)
	cl.ssh = sshClient
	return cl, nil
}

// authMethods builds the auth list. x/crypto/ssh tries each method type
// only once, so all keys (agent + files) go into a single PublicKeys method.
func authMethods(r config.Remote, p Prompter, st *authState, tr *Trace) ([]ssh.AuthMethod, func(), error) {
	closeAgent := func() {}
	var signers []ssh.Signer
	inAgent := map[string]bool{}

	if conn, err := dialAgent(); err == nil {
		closeAgent = func() { conn.Close() }
		if s, err := agent.NewClient(conn).Signers(); err == nil {
			for _, k := range s {
				signers = append(signers, tr.signer(k, "ssh-agent key"))
				inAgent[string(k.PublicKey().Marshal())] = true
			}
		}
	}

	files, err := identityFiles(r.IdentityFile)
	if err != nil {
		return nil, closeAgent, err
	}
	for _, f := range files {
		if st.skipped[f] {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			if r.IdentityFile != "" {
				return nil, closeAgent, fmt.Errorf("identity_file: %w", err)
			}
			continue // default key that does not exist
		}
		s, err := ssh.ParsePrivateKey(data)
		var missing *ssh.PassphraseMissingError
		switch {
		case err == nil:
			if !inAgent[string(s.PublicKey().Marshal())] {
				signers = append(signers, tr.signer(s, "key file "+f))
			}
		case errors.As(err, &missing):
			pub := missing.PublicKey
			if pub == nil {
				pub = readPublicKey(f + ".pub") // old PEM keys do not embed it
			}
			if pub != nil {
				if !inAgent[string(pub.Marshal())] { // otherwise the agent already holds it
					signers = append(signers, tr.signer(&lazySigner{pub: pub, path: f, data: data, p: p, st: st}, "key file "+f))
				}
				continue
			}
			// Without the public key the server cannot be asked first, so the
			// passphrase has to be asked now (empty skips the key).
			s, err := st.unlock(p, f, data)
			if err != nil {
				return nil, closeAgent, err
			}
			if s != nil {
				signers = append(signers, tr.signer(s, "key file "+f))
			}
		default:
			if r.IdentityFile != "" {
				return nil, closeAgent, fmt.Errorf("%s: %w", f, err)
			}
		}
	}

	var methods []ssh.AuthMethod
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}
	// Ask for the password at most once, shared by "password" and
	// "keyboard-interactive" (servers differ in which one they offer) and by
	// reconnects.
	getPw := func() (string, error) {
		if !st.pwDone {
			st.pw, st.pwErr = p.Password(fmt.Sprintf("%s@%s's password: ", r.User, r.Host))
			st.pwDone = true
		}
		return st.pw, st.pwErr
	}
	methods = append(methods,
		ssh.PasswordCallback(func() (string, error) {
			tr.method("password")
			return getPw()
		}),
		ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			tr.method("password (keyboard-interactive)")
			answers := make([]string, len(questions))
			for i := range questions {
				a, err := getPw()
				if err != nil {
					return nil, err
				}
				answers[i] = a
			}
			return answers, nil
		}),
	)
	return methods, closeAgent, nil
}

func identityFiles(explicit string) ([]string, error) {
	if explicit != "" {
		p, err := config.ExpandHome(explicit)
		return []string{p}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	var out []string
	for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		out = append(out, filepath.Join(home, ".ssh", n))
	}
	return out, nil
}

// knownHostsPath returns ~/.ssh/known_hosts, creating it if needed.
func knownHostsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "known_hosts")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return "", err
	}
	f.Close()
	return p, nil
}

func hostKeyCallback(addr string, p Prompter) (ssh.HostKeyCallback, []string, error) {
	khPath, err := knownHostsPath()
	if err != nil {
		return nil, nil, err
	}
	return newHostKeyCallback(khPath, addr, p)
}

func newHostKeyCallback(khPath, addr string, p Prompter) (ssh.HostKeyCallback, []string, error) {
	base, skipped, err := loadKnownHosts(khPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", khPath, err)
	}
	for _, s := range skipped {
		p.Warn(fmt.Sprintf("%s line %d is ignored because it cannot be parsed (%s)", khPath, s.line, s.reason))
	}
	cb := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := base(hostname, remote, key)
		var ke *knownhosts.KeyError
		if !errors.As(err, &ke) {
			return err // nil (known and matching) or another error
		}
		if len(ke.Want) > 0 {
			w := ke.Want[0]
			return fmt.Errorf("HOST KEY MISMATCH for %s: the server's %s key (%s) differs from %s line %d. "+
				"This may be an attack; if the NAS was reinstalled, remove that line and retry",
				knownhosts.Normalize(hostname), key.Type(), ssh.FingerprintSHA256(key), khPath, w.Line)
		}
		ok, perr := p.Confirm(fmt.Sprintf(
			"The authenticity of host '%s' can't be established.\n%s key fingerprint is %s.\nTrust this host and add it to %s?",
			knownhosts.Normalize(hostname), key.Type(), ssh.FingerprintSHA256(key), khPath))
		if perr != nil {
			return perr
		}
		if !ok {
			return errors.New("host key not trusted")
		}
		return appendKnownHost(khPath, knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key))
	}
	return cb, knownAlgorithms(base, addr), nil
}

// appendKnownHost adds line to known_hosts on a line of its own, even when
// the file does not end with a newline; otherwise the new key would join the
// last line and be lost (or skipped) on the next load.
func appendKnownHost(khPath, line string) error {
	data, err := os.ReadFile(khPath)
	if err != nil {
		return err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		line = "\n" + line
	}
	f, err := os.OpenFile(khPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// skippedLine is a known_hosts line that loadKnownHosts could not parse.
type skippedLine struct {
	line   int
	reason string
}

// maxKnownHostsLine is the longest line (without its newline) the line
// scanner in knownhosts can read; a longer line fails the whole file.
const maxKnownHostsLine = bufio.MaxScanTokenSize - 1

// loadKnownHosts is knownhosts.New, except that a line it cannot parse (a key
// type x/crypto does not support, a line written by another tool) is skipped
// instead of failing the whole file. OpenSSH skips such lines too.
//
// knownhosts only reads files and stops at the first bad line, so the lines
// go into a temporary copy, and each bad line is blanked there in turn. Line
// numbers stay the same as in khPath; KnownKey.Filename names the copy.
func loadKnownHosts(khPath string) (ssh.HostKeyCallback, []skippedLine, error) {
	data, err := os.ReadFile(khPath)
	if err != nil {
		return nil, nil, err
	}
	lines := strings.SplitAfter(string(data), "\n")
	var skipped []skippedLine
	blank := func(i int, reason string) error {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "@revoked") {
			// Ignoring a revocation could accept a key the user meant to block.
			return fmt.Errorf("line %d: cannot parse a @revoked entry (%s); fix or remove that line", i+1, reason)
		}
		if strings.HasSuffix(lines[i], "\n") {
			lines[i] = "\n" // keep the line count
		} else {
			lines[i] = ""
		}
		skipped = append(skipped, skippedLine{line: i + 1, reason: reason})
		return nil
	}
	for i, l := range lines {
		if len(strings.TrimSuffix(l, "\n")) > maxKnownHostsLine {
			if err := blank(i, "line too long"); err != nil {
				return nil, nil, err
			}
		}
	}

	f, err := os.CreateTemp("", "camback-known_hosts-*")
	if err != nil {
		return nil, nil, err
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)

	prefix := "knownhosts: " + tmp + ":"
	for {
		if err := os.WriteFile(tmp, []byte(strings.Join(lines, "")), 0o600); err != nil {
			return nil, nil, err
		}
		cb, err := knownhosts.New(tmp)
		if err == nil {
			slices.SortFunc(skipped, func(a, b skippedLine) int { return a.line - b.line })
			return cb, skipped, nil
		}
		// Parse errors read "knownhosts: <file>:<line>: <reason>".
		rest, ok := strings.CutPrefix(err.Error(), prefix)
		num, reason, ok2 := strings.Cut(rest, ": ")
		n, nerr := strconv.Atoi(num)
		if !ok || !ok2 || nerr != nil || n < 1 || n > len(lines) || strings.TrimSpace(lines[n-1]) == "" {
			return nil, nil, err
		}
		if err := blank(n-1, reason); err != nil {
			return nil, nil, err
		}
	}
}

// knownAlgorithms asks known_hosts which key types it has for addr, so the
// server is asked for a key we can actually verify. Probing with a random
// key makes knownhosts report the stored keys in KeyError.Want.
func knownAlgorithms(base ssh.HostKeyCallback, addr string) []string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil
	}
	probe, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil
	}
	err = base(addr, &net.TCPAddr{IP: net.IPv4zero}, probe)
	var ke *knownhosts.KeyError
	if !errors.As(err, &ke) || len(ke.Want) == 0 {
		return nil // unknown host: let the library choose
	}
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, k := range ke.Want {
		switch t := k.Key.Type(); t {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA512)
			add(ssh.KeyAlgoRSASHA256)
			add(ssh.KeyAlgoRSA)
		default:
			add(t)
		}
	}
	return out
}
