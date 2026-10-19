package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/ssh"
)

const passphraseAttempts = 3

// authState is kept across reconnects within one Dial, so nothing is asked
// twice.
type authState struct {
	skipped  map[string]bool       // key files the user chose to skip
	unlocked map[string]ssh.Signer // decrypted keys, by file
	pw       string
	pwErr    error
	pwDone   bool
}

// keySkippedError aborts a handshake so Dial can reconnect without the key.
type keySkippedError struct{ path string }

func (e *keySkippedError) Error() string { return "skipped key " + e.path }

// unlock asks for a key's passphrase. An empty answer skips the key, as
// does a wrong passphrase three times; skipping returns (nil, nil).
// Only cancellation (Ctrl-C) is returned as an error.
func (st *authState) unlock(p Prompter, path string, data []byte) (ssh.Signer, error) {
	if s, ok := st.unlocked[path]; ok {
		return s, nil
	}
	prompt := fmt.Sprintf("Enter passphrase for key %s (empty to skip): ", path)
	for i := 0; i < passphraseAttempts; i++ {
		pass, err := p.Password(prompt)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			return nil, nil // e.g. not a terminal: skip the key
		}
		if pass == "" {
			return nil, nil
		}
		s, err := ssh.ParsePrivateKeyWithPassphrase(data, []byte(pass))
		if err == nil {
			st.unlocked[path] = s
			return s, nil
		}
		prompt = fmt.Sprintf("Wrong passphrase, try again for key %s (empty to skip): ", path)
	}
	return nil, nil
}

// lazySigner holds an encrypted key and asks for its passphrase only when
// the server has accepted its public key, i.e. when a signature is actually
// needed. x/crypto/ssh first asks the server whether a public key is
// acceptable and calls Sign only for keys it accepts.
type lazySigner struct {
	pub  ssh.PublicKey
	path string
	data []byte
	p    Prompter
	st   *authState
}

var _ ssh.AlgorithmSigner = (*lazySigner)(nil)

func (s *lazySigner) PublicKey() ssh.PublicKey { return s.pub }

func (s *lazySigner) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	return s.SignWithAlgorithm(rand, data, "")
}

// SignWithAlgorithm is needed so RSA keys can sign with rsa-sha2-256/512
// instead of the obsolete SHA-1 "ssh-rsa".
func (s *lazySigner) SignWithAlgorithm(rand io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	inner, err := s.st.unlock(s.p, s.path, s.data)
	if err != nil {
		return nil, err
	}
	if inner == nil {
		return nil, &keySkippedError{path: s.path}
	}
	if as, ok := inner.(ssh.AlgorithmSigner); ok && algorithm != "" {
		return as.SignWithAlgorithm(rand, data, algorithm)
	}
	return inner.Sign(rand, data)
}

// readPublicKey reads an OpenSSH ".pub" file, or returns nil.
func readPublicKey(path string) ssh.PublicKey {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil
	}
	return pub
}
