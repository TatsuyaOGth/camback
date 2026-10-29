package sftp

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/TatsuyaOGth/camback/internal/config"
)

// Stage is one step of connecting, in the order DialTrace takes them.
type Stage int

const (
	StageResolve   Stage = iota + 1 // look up the host name
	StageConnect                    // open the TCP connection
	StageHandshake                  // agree on SSH algorithms with the server
	StageHostKey                    // check the server's key against known_hosts
	StageAuthStart                  // the server goes on to the login (fails when it drops the connection first)
	StageAuth                       // log in
	StageSFTP                       // start the SFTP subsystem
)

// Trace records how far DialTrace got and how, for "camback doctor".
//
// Its methods do nothing on a nil *Trace, so Dial passes nil and behaves
// exactly as before: no extra name lookup, no wrapped keys.
type Trace struct {
	Addrs        []string      // addresses the host name resolved to
	Remote       string        // address the TCP connection reached
	HostKey      ssh.PublicKey // the server's host key, once it was shown
	HostKeyAdded bool          // the user trusted it just now; it was added to known_hosts
	Auth         string        // how the login succeeded, e.g. "password"
	Failed       Stage         // the stage that failed; 0 when DialTrace succeeded

	authTried bool // a password was asked or a key the server accepted was used
}

// DialTrace is Dial that records each stage in tr.
func DialTrace(ctx context.Context, r config.Remote, p Prompter, tr *Trace) (*Client, error) {
	return dial(ctx, r, p, tr)
}

// start clears what an earlier attempt of the same Dial recorded (one that
// skipped a key).
func (t *Trace) start() {
	if t != nil {
		t.Failed, t.Auth, t.authTried = 0, "", false
	}
}

// connected records the address the TCP connection reached.
func (t *Trace) connected(conn net.Conn) {
	if t != nil {
		t.Remote = conn.RemoteAddr().String()
	}
}

func (t *Trace) fail(s Stage, err error) error {
	if t != nil {
		t.Failed = s
	}
	return err
}

// resolve looks up host only to report it; the connection resolves it
// again, as it does without a trace.
func (t *Trace) resolve(ctx context.Context, host string) error {
	if t == nil {
		return nil
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return t.fail(StageResolve, fmt.Errorf("resolve %s: %w", host, err))
	}
	t.Addrs = addrs
	return nil
}

// prompter notes in t when the user trusts a new host key: within
// hostKeyCallback, Confirm asks nothing else.
func (t *Trace) prompter(p Prompter) Prompter {
	if t == nil {
		return p
	}
	return &trustRecorder{Prompter: p, t: t}
}

type trustRecorder struct {
	Prompter
	t *Trace
}

func (r *trustRecorder) Confirm(prompt string) (bool, error) {
	ok, err := r.Prompter.Confirm(prompt)
	if ok && err == nil {
		r.t.HostKeyAdded = true
	}
	return ok, err
}

// hostKey records the key cb is given and whether cb accepted it. checked
// reports whether the server got that far.
func (t *Trace) hostKey(cb ssh.HostKeyCallback) (_ ssh.HostKeyCallback, checked func() (bool, error)) {
	if t == nil {
		return cb, func() (bool, error) { return false, nil }
	}
	var called bool
	var cbErr error
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		called = true
		t.HostKey = key
		cbErr = cb(hostname, remote, key)
		return cbErr
	}, func() (bool, error) { return called, cbErr }
}

// handshakeFailed sorts a failed SSH handshake into the stage it failed at.
// After the host key, it is the login only if a login method was tried:
// otherwise the server dropped the connection first (a block list, too many
// connections). x/crypto/ssh tells that no method worked only by its message.
func (t *Trace) handshakeFailed(checked func() (bool, error), err error) error {
	switch called, cbErr := checked(); {
	case !called:
		return t.fail(StageHandshake, err)
	case cbErr != nil:
		return t.fail(StageHostKey, err)
	case t.authTried || strings.Contains(err.Error(), "unable to authenticate"):
		return t.fail(StageAuth, err)
	default:
		return t.fail(StageAuthStart, err)
	}
}

// signer makes s note how the login went when the server accepts it.
// x/crypto/ssh only signs with a key the server has accepted, so the last
// method noted is the one that logged in.
func (t *Trace) signer(s ssh.Signer, how string) ssh.Signer {
	if t == nil {
		return s
	}
	as, ok := s.(ssh.AlgorithmSigner)
	if !ok {
		return s // not one camback creates; left as it is rather than lose RSA SHA-2
	}
	desc := fmt.Sprintf("%s (%s %s)", how, s.PublicKey().Type(), ssh.FingerprintSHA256(s.PublicKey()))
	ts := &tracedSigner{AlgorithmSigner: as, t: t, desc: desc}
	if ms, ok := s.(ssh.MultiAlgorithmSigner); ok {
		return &tracedMultiSigner{tracedSigner: ts, algos: ms.Algorithms}
	}
	return ts
}

// method notes that the login is being tried with how (a password).
func (t *Trace) method(how string) {
	if t != nil {
		t.Auth, t.authTried = how, true
	}
}

type tracedSigner struct {
	ssh.AlgorithmSigner
	t    *Trace
	desc string
}

func (s *tracedSigner) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	s.t.authTried = true
	sig, err := s.AlgorithmSigner.Sign(rand, data)
	if err == nil {
		s.t.Auth = s.desc
	}
	return sig, err
}

func (s *tracedSigner) SignWithAlgorithm(rand io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	s.t.authTried = true
	sig, err := s.AlgorithmSigner.SignWithAlgorithm(rand, data, algorithm)
	if err == nil {
		s.t.Auth = s.desc
	}
	return sig, err
}

// tracedMultiSigner keeps the algorithms a MultiAlgorithmSigner is limited to.
type tracedMultiSigner struct {
	*tracedSigner
	algos func() []string
}

func (s *tracedMultiSigner) Algorithms() []string { return s.algos() }
