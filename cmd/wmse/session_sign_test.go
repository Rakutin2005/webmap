package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"apimap/internal/wmse"
)

// keyIn writes an RSA key into a session's directory, which is where a verifier
// looks for one: a test that put the key anywhere else would be testing the search
// rather than the signature.
func keyIn(t *testing.T, dir string) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "key.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// signedSession is a session over a file that has been signed, with the key beside
// it.
func signedSession(t *testing.T) (*session, string, *bytes.Buffer, string) {
	t.Helper()
	path := sessionFixture(t)
	key := keyIn(t, filepath.Dir(path))
	sig, err := wmse.SignFile(path, wmse.SignRSA, key)
	if err != nil {
		t.Fatalf("SignFile: %v", err)
	}
	s := openSession(t, path)
	if err := s.reload(&s.files[0]); err != nil {
		t.Fatalf("reload: %v", err)
	}
	var out bytes.Buffer
	s.out = &out
	return s, key, &out, sig.KeyID
}

// TestSignSignsTheFileTheSessionHasOpen, and says who signed it.
func TestSignSignsTheFileTheSessionHasOpen(t *testing.T) {
	path := sessionFixture(t)
	key := keyIn(t, filepath.Dir(path))
	s := openSession(t, path)
	var out bytes.Buffer
	s.out = &out

	s.cmdSign([]string{"rsa", key})
	got := out.String()
	if !strings.Contains(got, "signed") {
		t.Errorf("sign said nothing about signing:\n%s", got)
	}
	if !strings.Contains(got, "key.pem") {
		t.Errorf("sign did not name the key:\n%s", got)
	}
	// And the file on disk really does verify, which is the only claim that matters.
	if _, err := wmse.VerifyFile(path); err != nil {
		t.Errorf("the file does not verify after sign: %v", err)
	}
	// And the session's own view of the file knows, so `info` does not describe the
	// file as it was before the command that just changed it.
	if s.files[0].snap.Signature == nil {
		t.Errorf("the session's copy of the file does not know it is signed")
	}
}

// TestSignNoneRemovesTheSignature: removing a claim is not the same as leaving a
// broken one, and `sign none` is how a person says so.
func TestSignNoneRemovesTheSignature(t *testing.T) {
	s, _, out, _ := signedSession(t)
	s.cmdSign([]string{"none", ""})
	if !strings.Contains(out.String(), "removed") {
		t.Errorf("sign none said nothing:\n%s", out.String())
	}
	if _, err := wmse.VerifyFile(s.files[0].path); err != wmse.ErrUnsigned {
		t.Errorf("after sign none the file reports %v, want %v", err, wmse.ErrUnsigned)
	}
	if s.files[0].snap.Signature != nil {
		t.Errorf("the session's copy still claims a signature")
	}
}

// TestSignRefusesAnEncryptedFile: a signature over bytes nobody can see would need
// the file decrypted before it could be checked, which is a rule worth saying rather
// than discovering.
func TestSignRefusesAnEncryptedFile(t *testing.T) {
	s, _, out, _ := signedSession(t)
	if err := wmse.SealFile(s.files[0].path, wmse.SealPassword, "pw"); err != nil {
		t.Fatal(err)
	}
	key := keyIn(t, filepath.Dir(s.files[0].path))
	out.Reset()
	s.cmdSign([]string{"rsa", key})
	if !strings.Contains(out.String(), "encrypted") {
		t.Errorf("signing an encrypted file was not refused:\n%s", out.String())
	}
}

// TestSignRefusesAMethodItDoesNotHave, rather than guessing one.
func TestSignRefusesAMethodItDoesNotHave(t *testing.T) {
	s := openSession(t, sessionFixture(t))
	var out bytes.Buffer
	s.out = &out
	s.cmdSign([]string{"md5", "key.pem"})
	if !strings.Contains(out.String(), "unknown signing method") {
		t.Errorf("an unknown method was not refused:\n%s", out.String())
	}
	// And no arguments is a usage line rather than a panic.
	out.Reset()
	s.cmdSign(nil)
	if !strings.Contains(out.String(), "usage: sign") {
		t.Errorf("sign with no arguments:\n%s", out.String())
	}
}

// TestEditingASignedFileAsksFirst is the point of a signature: a file that was
// signed and then changed is not evidence of anything the signer vouched for, so the
// write stops and asks.
func TestEditingASignedFileAsksFirst(t *testing.T) {
	s, _, out, _ := signedSession(t)
	s.interactive = true
	s.keys = newPipeSource(strings.NewReader("n\n"))

	if s.warnBeforeEdit(&s.files[0]) {
		t.Errorf("a signed file was written without being asked about")
	}
	got := out.String()
	if !strings.Contains(got, "signed") || !strings.Contains(got, "invalid") {
		t.Errorf("the warning did not say what would happen:\n%s", got)
	}
	if !strings.Contains(got, "not re-made") {
		t.Errorf("the warning did not say the signature would not be re-made:\n%s", got)
	}
	// Answering yes lets it through, and the answer is remembered: being asked once
	// is a decision, being asked about every save after it is a nuisance.
	out.Reset()
	s.keys = newPipeSource(strings.NewReader("y\n"))
	if !s.warnBeforeEdit(&s.files[0]) {
		t.Errorf("answering yes did not let the write through")
	}
	out.Reset()
	s.keys = newPipeSource(strings.NewReader(""))
	if !s.warnBeforeEdit(&s.files[0]) {
		t.Errorf("the answer was asked for again after it was given")
	}
	if out.String() != "" {
		t.Errorf("being asked twice printed something: %q", out.String())
	}
}

// TestAnUnsignedFileIsNotWarnedAbout: the warning is for signed files, and a
// session on an ordinary scan must not stop asking about every save.
func TestAnUnsignedFileIsNotWarnedAbout(t *testing.T) {
	s := openSession(t, sessionFixture(t))
	if !s.warnBeforeEdit(&s.files[0]) {
		t.Errorf("an unsigned file stopped a write")
	}
}

// TestAFileWithNoTerminalIsNotWrittenTo: a warning nobody can answer is not a
// warning, it is a hang - so a scripted session answers no.
func TestAFileWithNoTerminalIsNotWrittenTo(t *testing.T) {
	s, _, _, _ := signedSession(t)
	s.interactive = false
	if s.warnBeforeEdit(&s.files[0]) {
		t.Errorf("a session with no terminal wrote to a signed file without asking")
	}
}

// TestEncryptSealsTheFileAndSaysSo: this is the one command that takes the file
// away from the person who opened it, so it says that and asks.
func TestEncryptSealsTheFileAndSaysSo(t *testing.T) {
	s, _, out, _ := signedSession(t)
	s.interactive = true
	s.keys = newPipeSource(strings.NewReader("y\n"))

	s.cmdEncrypt([]string{"password", "hunter2"})
	path := s.files[0].path
	sealed, alg := wmse.SealedState(path)
	if !sealed {
		t.Fatalf("the file on disk is not sealed")
	}
	if alg != wmse.SealPassword {
		t.Errorf("the file says it was sealed with %v", alg)
	}
	if !strings.Contains(out.String(), "cannot be read without it") {
		t.Errorf("encrypt did not say what it was about to do:\n%s", out.String())
	}
	// And it comes back with the password.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wmse.Unseal(raw, "hunter2"); err != nil {
		t.Errorf("the file does not open with the password it was sealed with: %v", err)
	}
}

// TestEncryptAsksBeforeItDoes: "no" is an answer, and the file is left alone.
func TestEncryptAsksBeforeItDoes(t *testing.T) {
	s, _, out, _ := signedSession(t)
	before, err := os.ReadFile(s.files[0].path)
	if err != nil {
		t.Fatal(err)
	}
	s.interactive = true
	s.keys = newPipeSource(strings.NewReader("n\n"))

	s.cmdEncrypt([]string{"password", "hunter2"})
	after, _ := os.ReadFile(s.files[0].path)
	if !bytes.Equal(before, after) {
		t.Errorf("the file was encrypted after being told no")
	}
	if !strings.Contains(out.String(), "not encrypted") {
		t.Errorf("answering no was not reported:\n%s", out.String())
	}
}

// TestEncryptWarnsThatAPasswordOnTheCommandLineIsInTheHistory: it is, and saying so
// is the only chance anybody gets to change it.
func TestEncryptWarnsThatAPasswordOnTheCommandLineIsInTheHistory(t *testing.T) {
	s, _, out, _ := signedSession(t)
	s.interactive = false
	s.cmdEncrypt([]string{"password", "hunter2"})
	if !strings.Contains(out.String(), "history") {
		t.Errorf("nothing said about the password being in the shell history:\n%s", out.String())
	}
}

// TestAKeylessSealedFileSaysWhatItNeeds, rather than failing in a way that leaves a
// person guessing.
func TestAKeylessSealedFileSaysWhatItNeeds(t *testing.T) {
	path := sessionFixture(t)
	if err := wmse.SealFile(path, wmse.SealPassword, "pw"); err != nil {
		t.Fatal(err)
	}
	// The command line has nobody to ask, so it says so and names the command that
	// can.
	_, _, err := openSnapshot(path, "", false)
	if err == nil {
		t.Fatalf("a sealed file opened with no key and no terminal")
	}
	if !strings.Contains(err.Error(), "wmse select") {
		t.Errorf("the refusal does not say what would work: %v", err)
	}
}

// TestAKeySealedFileOpensWithI: the one thing the -i flag is for.
func TestAKeySealedFileOpensWithI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scan.wmse")
	if _, err := wmse.Write(path, testSnapshot(), wmse.DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	// A 32-byte key file, which is what an AES-256 key is.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "aes.key")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := wmse.SealFile(path, wmse.SealAES, keyPath); err != nil {
		t.Fatal(err)
	}
	// Without the key it says what it needs.
	if _, _, err := openSnapshot(path, "", false); err == nil {
		t.Errorf("a sealed file opened with no key")
	}
	snap, info, err := openSnapshot(path, keyPath, false)
	if err != nil {
		t.Fatalf("the file did not open with the key: %v", err)
	}
	if len(snap.Links) != len(testSnapshot().Links) {
		t.Errorf("the decrypted file has %d links, want %d", len(snap.Links), len(testSnapshot().Links))
	}
	if info == nil || info.TotalBytes == 0 {
		t.Errorf("the header of the decrypted file is empty")
	}
}

// TestTakeKeyFlag covers the spellings, because a person who has used -o anywhere
// else will type -i=key and should not be surprised here.
func TestTakeKeyFlag(t *testing.T) {
	for _, c := range []struct {
		in   []string
		key  string
		rest string
	}{
		{[]string{"-i", "k", "file.wmse"}, "k", "file.wmse"},
		{[]string{"-i=k", "file.wmse"}, "k", "file.wmse"},
		{[]string{"--i=k", "file.wmse"}, "k", "file.wmse"},
		{[]string{"--i", "k", "file.wmse"}, "k", "file.wmse"},
		{[]string{"-r", "file.wmse"}, "", "-r file.wmse"},
		{[]string{"file.wmse", "-i", "k"}, "k", "file.wmse"},
		// A flag that ends in -i is not -i.
		{[]string{"-api", "file.wmse"}, "", "-api file.wmse"},
	} {
		key, rest := takeKeyFlag(c.in)
		if key != c.key {
			t.Errorf("takeKeyFlag(%q) key = %q, want %q", c.in, key, c.key)
		}
		if strings.Join(rest, " ") != c.rest {
			t.Errorf("takeKeyFlag(%q) rest = %q, want %q", c.in, rest, c.rest)
		}
	}
}

// TestCompleteCryptoOffersMethodsThenKeys: the first word after the command is a
// method and the second is a key, and offering the wrong one of the two would send
// somebody to look for a key file they meant to type a password into.
func TestCompleteCryptoOffersMethodsThenKeys(t *testing.T) {
	s := openSession(t, sessionFixture(t))
	if got := s.completeCrypto([]rune("sign "), 5, ""); len(got) != 4 {
		t.Errorf("completing a sign method offered %q", got)
	}
	if got := s.completeCrypto([]rune("encrypt "), 8, ""); len(got) != 3 {
		t.Errorf("completing an encrypt method offered %q", got)
	}
	// The second word is a key file, and the files offered are the ones in the
	// directory the word is relative to - which is the directory the person is in,
	// not the one the scan is in. A test has to say which it means.
	got := s.completeCrypto([]rune("sign rsa session_sign"), 13, "session_sign")
	if len(got) == 0 {
		t.Errorf("completing a key prefix offered nothing")
	}
	for _, name := range got {
		if !strings.Contains(filepath.Base(name), "session_sign") {
			t.Errorf("completing a key offered %q, which does not start with what was typed", name)
		}
	}
	// And a prefix nothing matches offers nothing rather than everything.
	if got := s.completeCrypto([]rune("sign rsa zzz"), 8, "zzz"); got != nil {
		t.Errorf("completing a key that is not there offered %q", got)
	}
}

// TestSignAndEncryptAreDiscoverable: a command that exists and is not in the help is
// a command nobody will find.
func TestSignAndEncryptAreDiscoverable(t *testing.T) {
	s := openSession(t, sessionFixture(t))
	var out bytes.Buffer
	s.out = &out
	s.help()
	for _, want := range []string{"sign", "encrypt", "ls -d", "read"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help does not mention %s:\n%s", want, out.String())
		}
	}
	for _, name := range commandNames {
		if name == "sign" || name == "encrypt" {
			continue
		}
		if !strings.Contains(out.String(), name) {
			t.Errorf("help does not mention %s", name)
		}
	}
}
