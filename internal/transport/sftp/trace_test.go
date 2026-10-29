package sftp

import (
	"crypto/rand"
	"crypto/rsa"
	"slices"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestTraceSignerKeepsAlgorithms(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if got := (*Trace)(nil).signer(s, "key file x"); got != s {
		t.Error("without a trace the signer must be used as it is")
	}

	limited, err := ssh.NewSignerWithAlgorithms(s.(ssh.AlgorithmSigner), []string{ssh.KeyAlgoRSASHA512})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := (&Trace{}).signer(limited, "key file x").(ssh.MultiAlgorithmSigner)
	if !ok {
		t.Fatal("a MultiAlgorithmSigner lost its algorithms when wrapped")
	}
	if a := got.Algorithms(); !slices.Equal(a, []string{ssh.KeyAlgoRSASHA512}) {
		t.Errorf("algorithms = %q", a)
	}

	tr := &Trace{}
	if _, err := tr.signer(s, "key file x").(ssh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, []byte("data"), ssh.KeyAlgoRSASHA256); err != nil {
		t.Fatal(err)
	}
	if tr.Auth == "" || !tr.authTried {
		t.Errorf("signing was not recorded: %+v", tr)
	}
}
