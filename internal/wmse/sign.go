package wmse

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A Signature is what a file says about itself: who signed it, with what, and
// the signature itself.
//
// The signature covers the file's own bytes with the signature section left out -
// not the sections re-encoded, and not the snapshot it was built from. Signing an
// encoding instead would mean a signature that a later version of the writer could
// invalidate without changing a single fact in the file, and a verifier that had to
// re-marshal the file before it could check anything. Skipping one section's bytes
// is a definition that holds for any writer, which is the only kind that is worth
// having in a format.
type Signature struct {
	// Alg is how the signature was made. It is recorded rather than inferred,
	// because "does this verify" and "was this made the way I think" are different
	// questions, and a verifier that guessed would answer the second one.
	Alg SignAlg
	// Signer is the key's identity: a fingerprint for a key the reader can look up,
	// and the file's name for one it cannot. It is what a person reads when they
	// are deciding whether to trust the file.
	Signer string
	// KeyID is a digest of the public key, so two signatures made with the same key
	// name the same thing and a file signed with a different key is visibly so.
	KeyID string
	// When is when the signature was made. A signature has no time of its own, and a
	// file that says who signed it but not when is asking to be believed about the
	// least checkable part.
	When time.Time
	// KeyName is the name of the key file the signature was made with, so a verifier
	// knows where to look. It is a name and not a path on purpose: a file that
	// travels to another machine must not carry a path that means nothing there.
	KeyName string
	// Sig is the signature itself: PSS over SHA-256 for a key, or a detached gpg
	// signature for gpg.
	Sig []byte
}

// SignAlg is how a file was signed.
type SignAlg uint8

const (
	// SignNone is the absence of a signature, and what `sign none` leaves behind.
	SignNone SignAlg = iota
	// SignRSA is RSA-PSS over SHA-256 with a PEM private key.
	SignRSA
	// SignX509 is the same signature, made with the key inside a PEM certificate
	// and named by that certificate's subject.
	SignX509
	// SignGPG is a detached OpenPGP signature, made by gpg and verified by it.
	SignGPG
)

// SignAlgName is what a person reads.
func (a SignAlg) String() string {
	switch a {
	case SignRSA:
		return "rsa"
	case SignX509:
		return "x509"
	case SignGPG:
		return "gpg"
	}
	return "none"
}

// ParseSignAlg reads an algorithm the way a person writes it.
func ParseSignAlg(s string) (SignAlg, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "rsa":
		return SignRSA, nil
	case "x509", "cert", "certificate":
		return SignX509, nil
	case "gpg", "pgp", "openpgp":
		return SignGPG, nil
	case "none", "":
		return SignNone, nil
	}
	return SignNone, fmt.Errorf("unknown signing method %q: rsa, x509, gpg or none", s)
}

// ErrUnsigned is what a file with no signature says about itself.
var ErrUnsigned = errors.New("this file is not signed")

// SignFile signs a file and rewrites it with the signature inside.
//
// Signing is a fixed point, and the reason is worth stating because it is the whole
// difficulty. The signature covers the file's bytes with the signature section taken
// out, and the signature section's size is itself part of the file - so the size of
// the answer changes the question. The way out is to marshal, sign, and if the
// signature came out a different length, marshal again: the size settles in a
// couple of rounds because a signature's length is a property of the key, not of
// the bytes it covers. A file that would not settle is refused rather than signed
// with a signature that does not match it.
func SignFile(path string, alg SignAlg, keyPath string) (*Signature, error) {
	if alg == SignNone {
		return nil, nil
	}
	f, err := Open(path)
	if err != nil {
		return nil, err
	}
	snap, err := f.Load()
	if err != nil {
		return nil, err
	}
	sig, err := signSnapshot(snap, alg, keyPath)
	if err != nil {
		return nil, err
	}
	snap.Signature = sig
	if _, err := Write(path, snap, DefaultOptions()); err != nil {
		return nil, err
	}
	return sig, nil
}

// signSnapshot finds the signature over a snapshot's own file image.
func signSnapshot(snap *Snapshot, alg SignAlg, keyPath string) (*Signature, error) {
	// Everything but the signature itself is known before signing, and it is what a
	// reader reads first, so it is filled in once and kept.
	meta, err := signatureMeta(alg, keyPath)
	if err != nil {
		return nil, err
	}
	prev := -1
	for attempt := 0; attempt < 8; attempt++ {
		// The placeholder has to be a real section of the right shape for the image
		// to be the one the signature will be checked against.
		snap.Signature = meta
		raw, _, err := Marshal(snap, DefaultOptions())
		if err != nil {
			return nil, err
		}
		signed, _, err := signedBytes(raw)
		if err != nil {
			return nil, err
		}
		var keyID string
		meta.Sig, keyID, err = signBytes(alg, keyPath, signed)
		if err != nil {
			return nil, err
		}
		if keyID != "" {
			meta.KeyID = keyID
		}
		if len(meta.Sig) == prev {
			return meta, nil
		}
		prev = len(meta.Sig)
	}
	return nil, errors.New("the signature's size did not settle, so the file cannot be signed consistently")
}

// signatureMeta is the part of a signature that does not depend on the bytes being
// signed: who, with what, and when. A gpg signature's identity comes out of the
// signature itself, so that one field is filled in afterwards.
func signatureMeta(alg SignAlg, keyPath string) (*Signature, error) {
	meta := &Signature{Alg: alg, When: time.Now().UTC()}
	if keyPath != "" {
		meta.KeyName = filepath.Base(keyPath)
	}
	switch alg {
	case SignRSA, SignX509:
		raw, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, err
		}
		priv, cert, err := parsePrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", keyPath, err)
		}
		if priv == nil && cert != nil {
			// Said plainly, because "it did not work" would send somebody looking for
			// a problem with the key when the problem is that there is no key in the
			// file.
			return nil, fmt.Errorf("%s holds a certificate and no private key: a certificate is the public half, and signing needs the private one. Put both in the file, or use rsa with the key", keyPath)
		}
		if _, ok := priv.(*rsa.PrivateKey); !ok {
			return nil, fmt.Errorf("%s: only an RSA key can make an rsa or x509 signature here", keyPath)
		}
		switch {
		case cert != nil && cert.Subject.String() != "":
			meta.Signer = cert.Subject.String()
		case cert != nil:
			meta.Signer = "certificate " + filepath.Base(keyPath)
		case alg == SignX509:
			meta.Signer = "key " + filepath.Base(keyPath)
		default:
			meta.Signer = "rsa " + filepath.Base(keyPath)
		}
	case SignGPG:
		if _, err := exec.LookPath("gpg"); err != nil {
			return nil, errors.New("gpg is not installed, so a gpg signature cannot be made here")
		}
	}
	return meta, nil
}

// signBytes makes the signature itself over the signed data, and says which key
// made it. The key's identity comes back separately because for gpg it can only be
// read out of the signature gpg produced, and a caller that had to sign twice to
// learn it would be doing the awkward thing twice.
func signBytes(alg SignAlg, keyPath string, signed []byte) (sig []byte, keyID string, err error) {
	digest := sha256.Sum256(signed)
	switch alg {
	case SignRSA, SignX509:
		raw, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, "", err
		}
		priv, _, err := parsePrivateKey(raw)
		if err != nil {
			return nil, "", err
		}
		rsaKey, ok := priv.(*rsa.PrivateKey)
		if !ok {
			return nil, "", fmt.Errorf("%s: only an RSA key can make an rsa or x509 signature here", keyPath)
		}
		sig, err = rsa.SignPSS(rand.Reader, rsaKey, crypto.SHA256, digest[:], nil)
		if err != nil {
			return nil, "", err
		}
		sum := sha256.Sum256(rsaKey.N.Bytes())
		return sig, fmt.Sprintf("%x", sum[:8]), nil
	case SignGPG:
		sig, err = gpgDetachSign(signed, keyPath)
		if err != nil {
			return nil, "", err
		}
		return sig, gpgFingerprint(sig), nil
	}
	return nil, "", fmt.Errorf("unknown signing method %d", alg)
}

// signWithKey signs a digest with a PEM key, which is either the key itself or a
// certificate holding it. The two are told apart by what the file parses as, rather
// than by the method asked for, so a method and a key that disagree produce an
// error instead of a signature nobody can check.
func signWithKey(alg SignAlg, keyPath string, digest []byte) (sig []byte, signer, keyID string, err error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, "", "", err
	}
	priv, cert, err := parsePrivateKey(raw)
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: %w", keyPath, err)
	}
	if priv == nil && cert != nil {
		// Said plainly, because "it did not work" would send somebody looking for a
		// problem with the key when the problem is that there is no key in the file.
		return nil, "", "", fmt.Errorf("%s holds a certificate and no private key: a certificate is the public half, and signing needs the private one. Put both in the file, or use `rsa` with the key", keyPath)
	}
	rsaKey, ok := priv.(*rsa.PrivateKey)
	if !ok {
		return nil, "", "", fmt.Errorf("%s: only an RSA key can make an rsa or x509 signature here", keyPath)
	}
	sig, err = rsa.SignPSS(rand.Reader, rsaKey, crypto.SHA256, digest, nil)
	if err != nil {
		return nil, "", "", err
	}
	sum := sha256.Sum256(rsaKey.PublicKey.N.Bytes())
	keyID = fmt.Sprintf("%x", sum[:8])
	switch {
	case cert != nil:
		signer = cert.Subject.String()
		if signer == "" {
			signer = filepath.Base(keyPath)
		}
	case alg == SignX509:
		// An x509 signature whose key came from a bare key file has no certificate
		// to name, and saying so is better than inventing a subject.
		signer = "key " + filepath.Base(keyPath)
	default:
		signer = "rsa " + filepath.Base(keyPath)
	}
	return sig, signer, keyID, nil
}

// parsePrivateKey accepts a PEM private key or a PEM certificate, in either PKCS#1
// or PKCS#8, and returns whichever it found. A certificate is a wrapper around a
// key, so a person who was handed a certificate expects signing to work.
func parsePrivateKey(raw []byte) (crypto.Signer, *x509.Certificate, error) {
	rest := raw
	// A certificate is a public credential: it says who a key belongs to and it
	// does not contain the key. So a file may hold one and still hold the key
	// beside it, and the two are found together rather than one replacing the
	// other - which is what makes a bundle work and a bare certificate fail with
	// an honest reason.
	var cert *x509.Certificate
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			if cert != nil {
				return nil, cert, nil
			}
			return nil, nil, errors.New("no PEM block: expected a private key or a certificate")
		}
		switch block.Type {
		case "RSA PRIVATE KEY":
			k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			// The certificate found earlier in the same file comes back with the
			// key, because a bundle is exactly that and dropping half of it would
			// make a signed certificate name a file instead of a subject.
			return k, cert, err
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, nil, err
			}
			signer, ok := k.(crypto.Signer)
			if !ok {
				return nil, nil, errors.New("that key cannot sign")
			}
			return signer, cert, nil
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, nil, err
			}
			cert = c
			continue
		}
		// An encrypted key or a certificate request is not something to guess at:
		// the message would have to be a passphrase prompt, and this is a file
		// being read in a loop with nothing to prompt on.
		if strings.Contains(block.Type, "ENCRYPTED") {
			return nil, nil, fmt.Errorf("%s is an encrypted %s: decrypt it first", block.Type, "key")
		}
	}
}

// gpgDetachSign makes a detached OpenPGP signature over the bytes.
//
// gpg is run rather than reimplemented. OpenPGP is a large standard with a long
// history of interoperability traps, and a signature that only this tool can verify
// is not a signature - it is a checksum with a key attached. The bytes are handed
// over on stdin and the signature comes back on stdout, so nothing is written to
// the disk to be cleaned up, and the process is given no terminal of its own.
func gpgDetachSign(data []byte, keyPath string) ([]byte, error) {
	if _, err := exec.LookPath("gpg"); err != nil {
		return nil, errors.New("gpg is not installed, so a gpg signature cannot be made here")
	}
	args := []string{"--batch", "--yes", "--armor", "--detach-sign", "--local-user", keyPath}
	cmd := exec.Command("gpg", args...)
	cmd.Stdin = bytes.NewReader(data)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("gpg: %s", lastLine(msg))
	}
	if out.Len() == 0 {
		return nil, errors.New("gpg produced no signature")
	}
	return out.Bytes(), nil
}

// gpgVerify asks gpg whether a detached signature matches the bytes, and says so
// in the tool's own words: gpg's exit status distinguishes a bad signature from a
// missing key, and that difference is the whole point of asking it rather than
// checking a digest.
func gpgVerify(data, sig []byte) error {
	if _, err := exec.LookPath("gpg"); err != nil {
		return errors.New("gpg is not installed, so a gpg signature cannot be checked here")
	}
	cmd := exec.Command("gpg", "--batch", "--verify", "-", "-")
	// gpg reads the signed data from stdin and the detached signature from the
	// file named "-", so both are fed on the same stream.
	cmd.Stdin = io.MultiReader(bytes.NewReader(data), bytes.NewReader(sig))
	var errOut bytes.Buffer
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		switch {
		case strings.Contains(msg, "BAD signature"):
			return errors.New("the signature does not match this file")
		case strings.Contains(msg, "No public key"), strings.Contains(msg, "Can't check signature"):
			return fmt.Errorf("gpg has no key to check this signature with: %s", lastLine(msg))
		}
		return fmt.Errorf("gpg: %s", lastLine(msg))
	}
	return nil
}

// gpgFingerprint is the signing subkey's fingerprint, read out of the armored
// signature. gpg already computed it, and reading it is better than inventing an
// identity for a key the file has only ever been signed with.
func gpgFingerprint(sig []byte) string {
	for _, line := range strings.Split(string(sig), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, " ") && strings.Contains(line, " ") {
			// The "Version:"/"Key:" header block gpg emits into the armor.
			if f := strings.TrimSpace(strings.TrimPrefix(line, "Key:")); f != line && len(f) == 40 {
				return f
			}
		}
	}
	sum := sha256.Sum256(sig)
	return fmt.Sprintf("%x", sum[:8])
}

// VerifyFile checks a file against its own signature and says which of the three
// answers it is: signed and valid, signed and not, or not signed at all.
//
// A file with no signature is not reported as invalid. It never claimed to be
// authentic, and telling a reader it is "not authentic" would be a different and
// much stronger claim than the one that is true.
func VerifyFile(path string) (*Signature, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sig, err := signatureOf(raw)
	if err != nil {
		return nil, err
	}
	if sig == nil {
		return nil, ErrUnsigned
	}
	if err := verifyAgainst(raw, sig, filepath.Dir(path)); err != nil {
		return sig, err
	}
	return sig, nil
}

// verifyAgainst checks one signature over the file's own bytes. dir is where the
// file lives, and is where a key is looked for.
func verifyAgainst(raw []byte, sig *Signature, dir string) error {
	signed, _, err := signedBytes(raw)
	if err != nil {
		return err
	}
	switch sig.Alg {
	case SignRSA, SignX509:
		return verifyWithKey(sig, signed, dir)
	case SignGPG:
		return gpgVerify(signed, sig.Sig)
	}
	return fmt.Errorf("the file names a signing method this reader does not know: %d", sig.Alg)
}

// verifyWithKey finds the public key for a signature and checks it.
//
// The key is looked for beside the file that carries the signature, by the name the
// signature recorded, because that is the only place a self-contained file can point
// at: a file that carried its own public key would be signing itself, which proves
// only that whoever wrote it had a key.
func verifyWithKey(sig *Signature, signed []byte, dir string) error {
	key, err := findPublicKey(sig, dir)
	if err != nil {
		return err
	}
	rsaKey, err := rsaPublicOf(key)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(rsaKey.N.Bytes())
	if want := fmt.Sprintf("%x", sum[:8]); want != sig.KeyID {
		return fmt.Errorf("the file was signed with a different key (%s, not %s)", want, sig.KeyID)
	}
	digest := sha256.Sum256(signed)
	return rsa.VerifyPSS(rsaKey, crypto.SHA256, digest[:], sig.Sig, nil)
}

func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return s
}

func nowUnix() int64 { return time.Now().UnixNano() }
