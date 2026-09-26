package wmse

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"apimap/internal/linker"
)

// testKeyPair writes a PEM RSA key pair and returns the two paths. Every signing
// test needs a real key, and generating one per test would make the suite mostly
// waiting.
func testKeyPair(t *testing.T) (privPath, pubPath string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	privPath = filepath.Join(dir, "key.pem")
	pubPath = filepath.Join(dir, "key.pub.pem")
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	return privPath, pubPath
}

// signedFixture is a file with a signature in it, and the key beside it - which is
// where a verifier looks, so a test that put the key anywhere else would be testing
// the wrong thing.
func signedFixture(t *testing.T, alg SignAlg) (path string, sig *Signature) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "scan.wmse")
	if _, err := Write(path, testSnapshot(), DefaultOptions()); err != nil {
		t.Fatalf("write: %v", err)
	}
	priv, _ := testKeyPair(t)
	raw, err := os.ReadFile(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// SignFile signs and writes in one step, and it does not attach the signature
	// a second time here: a load-and-rewrite is not guaranteed to be the byte for
	// byte image that was signed, and a test that did it would be testing the
	// round trip rather than the signature.
	sig, err = SignFile(path, alg, priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return path, sig
}

// attachSignature puts a signature into a file by rewriting it, which is all that
// signing a file does: the findings are untouched and one section is added.
func attachSignature(path string, sig *Signature) error {
	f, err := Open(path)
	if err != nil {
		return err
	}
	snap, err := f.Load()
	if err != nil {
		return err
	}
	snap.Signature = sig
	_, err = Write(path, snap, DefaultOptions())
	return err
}

// TestSignAndVerifyRsa is the whole round trip: a file is signed with a key, and
// the signature is checked against the file's own bytes.
func TestSignAndVerifyRsa(t *testing.T) {
	path, sig := signedFixture(t, SignRSA)

	got, err := VerifyFile(path)
	if err != nil {
		t.Fatalf("a file signed a moment ago does not verify: %v", err)
	}
	if got.KeyID != sig.KeyID || got.Alg != SignRSA {
		t.Errorf("verified against a different signature: %+v", got)
	}
	if got.Signer == "" {
		t.Errorf("the signature does not say who made it")
	}
	if got.When.IsZero() {
		t.Errorf("the signature does not say when")
	}
}

// TestTheSignatureSurvivesTheFileBeingRead: signing adds a section, and reading
// the file has to produce the same findings with the signature attached - or a
// signed file would be a file nothing else can open.
func TestTheSignatureSurvivesTheFileBeingRead(t *testing.T) {
	path, _ := signedFixture(t, SignRSA)
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := f.Load()
	if err != nil {
		t.Fatalf("a signed file does not load: %v", err)
	}
	if snap.Signature == nil {
		t.Fatalf("the signature was not read back")
	}
	if snap.Signature.Alg != SignRSA || len(snap.Signature.Sig) == 0 {
		t.Errorf("the signature came back wrong: %+v", snap.Signature)
	}
	if len(snap.Links) != len(testSnapshot().Links) {
		t.Errorf("signing changed the findings: %d links", len(snap.Links))
	}
}

// TestAnUnsignedFileIsNotAnInvalidOne: a file that was never signed is not a file
// with a broken signature, and telling a reader the second would be a much
// stronger and quite different accusation.
func TestAnUnsignedFileIsNotAnInvalidOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.wmse")
	if _, err := Write(path, testSnapshot(), DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFile(path); err != ErrUnsigned {
		t.Errorf("an unsigned file reported %v, want %v", err, ErrUnsigned)
	}
}

// TestAnEditedFileDoesNotVerify: the signature covers the file's bytes, so changing
// them is what a signature is for catching. This is the case a session's save makes,
// and it is why saving a signed file has to warn first.
func TestAnEditedFileDoesNotVerify(t *testing.T) {
	path, original := signedFixture(t, SignRSA)
	if _, err := VerifyFile(path); err != nil {
		t.Fatalf("the file should verify before it is edited: %v", err)
	}

	// The edit a session makes: another link, and the signature section dropped,
	// because a writer that knows nothing of it writes no section.
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	snap.Links = append(snap.Links, linker.Link{
		HREF: "/added", Resolved: "https://example.com/added",
		Domain: "example.com", Category: linker.CategoryWebPage,
		LinkType: linker.LinkTypeRelative, Depth: 1, SourceURL: "https://example.com/",
	})
	snap.Signature = nil
	if _, err := Write(path, snap, DefaultOptions()); err != nil {
		t.Fatal(err)
	}

	// The file now claims nothing at all, which is a different and honest answer.
	if _, err := VerifyFile(path); err != ErrUnsigned {
		t.Errorf("a file whose signature section was dropped reports %v, want %v", err, ErrUnsigned)
	}

	// And the signature that was over the old bytes does not match the new ones,
	// which is the half that catches a file that was changed and re-signed.
	if err := attachSignature(path, original); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFile(path); err == nil {
		t.Errorf("a signature over the old bytes verifies against an edited file")
	}
}

// TestASignatureFromADifferentKeyIsNotThisFiles: a file carries the identity of the
// key that signed it, so a signature made with another key is visibly not this
// file's - even if it is a perfectly good signature over these bytes.
func TestASignatureFromADifferentKeyIsNotThisFiles(t *testing.T) {
	path, _ := signedFixture(t, SignRSA)
	other, _ := testKeyPair(t)
	// The key that is beside the file is the one a verifier finds, so put the
	// *other* key's signature on the file and check it against the first.
	raw, _ := os.ReadFile(other)
	os.WriteFile(filepath.Join(filepath.Dir(path), "key.pem"), raw, 0o600)
	_ = raw
	// Sign with the first key, so the signature is good and the key beside the
	// file is the wrong one.
	firstKey := filepath.Join(t.TempDir(), "first.pem")
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	os.WriteFile(firstKey, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k),
	}), 0o600)
	if _, err := SignFile(path, SignRSA, firstKey); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFile(path); err == nil {
		t.Errorf("a signature made with a key the file does not name was accepted")
	}
}

// TestSealAndUnsealWithAPassword is the round trip for a password, including that
// the wrong password is refused rather than producing rubbish.
func TestSealAndUnsealWithAPassword(t *testing.T) {
	raw := []byte("a scan, or something that looks like one")
	sealed, err := Seal(raw, SealPassword, "correct horse")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !IsSealed(sealed) {
		t.Errorf("a sealed file does not say it is sealed")
	}
	if bytes.Contains(sealed, raw) {
		t.Errorf("the sealed file still contains what it was given")
	}
	alg, err := SealMethod(sealed)
	if err != nil || alg != SealPassword {
		t.Errorf("the method could not be read without the key: %v %v", alg, err)
	}
	back, err := Unseal(sealed, "correct horse")
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if !bytes.Equal(back, raw) {
		t.Errorf("unsealed to %q, want %q", back, raw)
	}
	// The wrong password is named, not left as a cipher failure to be decoded.
	if _, err := Unseal(sealed, "wrong horse"); err != ErrWrongPassword {
		t.Errorf("a wrong password gave %v, want %v", err, ErrWrongPassword)
	}
	// Two files with the same password are not the same file: the salt is random,
	// so the same input seals to different bytes.
	other, err := Seal(raw, SealPassword, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sealed, other) {
		t.Errorf("two seals of the same thing are identical, so the salt is not random")
	}
}

// TestSealAndUnsealWithAKeyFile: an AES key is used as it stands, and a key of the
// wrong length is refused rather than stretched into looking like a good one.
func TestSealAndUnsealWithAKeyFile(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, pbkdf2KeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "aes.key")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	raw := []byte("the bytes of a scan")
	sealed, err := Seal(raw, SealAES, keyPath)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	back, err := Unseal(sealed, keyPath)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if !bytes.Equal(back, raw) {
		t.Errorf("unsealed to %q", back)
	}
	// The wrong key does not open it.
	otherKey := filepath.Join(dir, "other.key")
	other := make([]byte, pbkdf2KeyLen)
	rand.Read(other)
	os.WriteFile(otherKey, other, 0o600)
	if _, err := Unseal(sealed, otherKey); err == nil {
		t.Errorf("a different key opened the file")
	}
	// A key of the wrong size is refused at the point of use.
	shortKey := filepath.Join(dir, "short.key")
	os.WriteFile(shortKey, key[:16], 0o600)
	if _, err := Seal(raw, SealAES, shortKey); err == nil {
		t.Errorf("a 16-byte key was accepted as an AES-256 key")
	}
}

// TestSealAndUnsealWithRSA: the content key is random per file, so two files sealed
// to the same public key are two unrelated files.
func TestSealAndUnsealWithRSA(t *testing.T) {
	priv, pub := testKeyPair(t)
	raw := []byte("a scan nobody else may read")
	sealed, err := Seal(raw, SealRSA, pub)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, raw) {
		t.Errorf("the sealed file still contains what it was given")
	}
	back, err := Unseal(sealed, priv)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if !bytes.Equal(back, raw) {
		t.Errorf("unsealed to %q", back)
	}
	// The public key cannot open it, which is the entire point of sealing to one.
	if _, err := Unseal(sealed, pub); err == nil {
		t.Errorf("the public key opened a file sealed to it")
	}
	other, _ := testKeyPair(t)
	if _, err := Unseal(sealed, other); err == nil {
		t.Errorf("an unrelated private key opened the file")
	}
}

// TestAnEditedSealedFileDoesNotOpen: the header is authenticated, so a header
// edited to name a weaker derivation is caught rather than obeyed.
func TestAnEditedSealedFileDoesNotOpen(t *testing.T) {
	sealed, err := Seal([]byte("x"), SealPassword, "pw")
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte of the header, which is the authenticated part.
	tampered := append([]byte(nil), sealed...)
	tampered[len(sealMagic)+2] ^= 0x40
	if _, err := Unseal(tampered, "pw"); err == nil {
		t.Errorf("a file with an edited header opened")
	}
	// And a flipped byte of the ciphertext is caught too.
	tampered = append([]byte(nil), sealed...)
	tampered[len(sealed)-1] ^= 1
	if _, err := Unseal(tampered, "pw"); err == nil {
		t.Errorf("a file with an edited body opened")
	}
}

// TestSealRefusesToSealTwice: a file that is already sealed is not sealed again,
// because the second seal would be of the first seal and the original would be
// unreachable rather than protected.
func TestSealRefusesToSealTwice(t *testing.T) {
	sealed, err := Seal([]byte("x"), SealPassword, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Seal(sealed, SealPassword, "pw"); err == nil {
		t.Errorf("a sealed file was sealed again")
	}
}

// TestSealFileInPlaceAndBack: the whole point of sealing a file is that the file on
// disk is the sealed one, and that it comes back byte for byte.
func TestSealFileInPlaceAndBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan.wmse")
	if _, err := Write(path, testSnapshot(), DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SealFile(path, SealPassword, "pw"); err != nil {
		t.Fatalf("SealFile: %v", err)
	}
	sealed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(sealed) {
		t.Fatalf("the file on disk is not sealed")
	}
	if bytes.Equal(sealed, before) {
		t.Errorf("sealing left the file as it was")
	}
	back, err := Unseal(sealed, "pw")
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if !bytes.Equal(back, before) {
		t.Errorf("the file did not come back as it was")
	}
}

// TestSealAlgNames: what a person types and what a person reads are the same words,
// because a method named two ways is a method somebody will get wrong.
func TestSealAlgNames(t *testing.T) {
	for _, name := range []string{"password", "rsa", "aes", "PASSWORD", " none "} {
		alg, err := ParseSealAlg(name)
		if err != nil {
			t.Errorf("ParseSealAlg(%q): %v", name, err)
			continue
		}
		if alg.String() != name2(alg) {
			t.Errorf("ParseSealAlg(%q) is %q, which is not what it says", name, alg)
		}
	}
	if _, err := ParseSealAlg("rot13"); err == nil {
		t.Errorf("an unknown method was accepted")
	}
}

// name2 is the canonical spelling of a sealing method, written out so the test
// compares the round trip against a fixed list rather than against the code it is
// testing.
func name2(a SealAlg) string {
	switch a {
	case SealPassword:
		return "password"
	case SealRSA:
		return "rsa"
	case SealAES:
		return "aes"
	}
	return "none"
}

func bigOne() *big.Int            { return big.NewInt(1) }
func pkixName(s string) pkix.Name { return pkix.Name{CommonName: s} }
func timeZero() time.Time         { return time.Unix(0, 0).UTC() }

// TestSignAlgNames: the same for signing.
func TestSignAlgNames(t *testing.T) {
	for _, name := range []string{"rsa", "x509", "gpg", "none", " RSA "} {
		alg, err := ParseSignAlg(name)
		if err != nil {
			t.Errorf("ParseSignAlg(%q): %v", name, err)
			continue
		}
		if alg.String() != strings.ToLower(strings.TrimSpace(name)) {
			t.Errorf("ParseSignAlg(%q) is %q", name, alg)
		}
	}
	if _, err := ParseSignAlg("md5"); err == nil {
		t.Errorf("an unknown signing method was accepted")
	}
	// `sign none` is how a signature is removed, and it is not an error.
	if alg, err := ParseSignAlg("none"); err != nil || alg != SignNone {
		t.Errorf("sign none is %v %v", alg, err)
	}
	if sig, err := SignFile("nowhere.wmse", SignNone, ""); err != nil || sig != nil {
		t.Errorf("sign none made a signature: %v %v", sig, err)
	}
}

// TestX509SigningUsesACertificate: a person handed a certificate expects signing to
// work, and the signature names the certificate rather than the file.
func TestX509SigningUsesACertificate(t *testing.T) {
	key, _ := testKeyPair(t)
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: bigOne(),
		Subject:      pkixName("scan signer"),
		NotBefore:    timeZero(),
		NotAfter:     timeZero().AddDate(10, 0, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	// A certificate is the public half and does not contain the key, so signing
	// with one needs the key beside it. That is what a bundle is, and it is why the
	// certificate alone has to fail with a reason.
	bundlePath := filepath.Join(dir, "bundle.pem")
	os.WriteFile(bundlePath, append(append([]byte{}, certPEM...), keyPEM...), 0o600)

	path := filepath.Join(dir, "scan.wmse")
	if _, err := Write(path, testSnapshot(), DefaultOptions()); err != nil {
		t.Fatal(err)
	}
	// The bundle is what a verifier finds beside the file.
	os.WriteFile(filepath.Join(dir, "bundle.pem"), mustRead(t, bundlePath), 0o600)
	sig, err := SignFile(path, SignX509, bundlePath)
	if err != nil {
		t.Fatalf("signing with a certificate: %v", err)
	}
	if _, err := VerifyFile(path); err != nil {
		t.Errorf("a file signed with a certificate does not verify: %v", err)
	}
	// The whole distinguished name, not just the common name: a certificate can
	// have two subjects with the same common name, and a signature that named only
	// the common one would not say which.
	if !strings.Contains(sig.Signer, "scan signer") {
		t.Errorf("the signature names %q, want the certificate's subject", sig.Signer)
	}

	// The certificate on its own is refused, and says why in terms somebody can act
	// on rather than "verification failed".
	certOnly := filepath.Join(dir, "certonly.pem")
	os.WriteFile(certOnly, certPEM, 0o644)
	_, err = SignFile(path, SignX509, certOnly)
	if err == nil {
		t.Errorf("a certificate with no key signed the file")
	} else if !strings.Contains(err.Error(), "no private key") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
	_ = key
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
