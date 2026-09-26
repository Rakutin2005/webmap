package wmse

import (
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// This file is the byte-level half of signing: where the signature section sits in
// a file, which bytes a signature covers, and where a verifier finds the key.
//
// Everything here works on the file's own bytes rather than on a snapshot, because
// a signature is a statement about bytes. Re-encoding the file to check it would
// make the answer depend on the writer as much as on the file, and a signature that
// a new version of the tool could invalidate without changing a fact in the file is
// not worth having.

// signedBytes is the data a signature covers: the file with its signature section's
// bytes taken out.
//
// Leaving the section out rather than zeroing it or moving it is what makes signing
// repeatable. The bytes being signed cannot contain the answer, so the answer can be
// replaced - a new signature, or none - without the data changing underneath it.
//
// The second return is the section that was removed, so a caller can say which bytes
// were skipped rather than leaving a reader to work it out.
func signedBytes(raw []byte) ([]byte, *SectionInfo, error) {
	f, err := Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	sec := findSection(f.Info(), SecSignature)
	if sec == nil {
		return raw, nil, nil
	}
	// A section's offset is counted from the first payload byte, not from the
	// start of the file, because the header does not know its own size until the
	// directory that describes it is laid out. So the range to skip is the header
	// plus the offset - and getting that wrong strips the right *number* of bytes
	// from the wrong place, which is a bug that produces a plausible-looking
	// digest rather than an obvious failure.
	start := int64(f.payloadBase()) + int64(sec.Offset)
	stored := int64(sec.StoredLen)
	if start < 0 || stored < 0 || start+stored > int64(len(raw)) {
		return nil, nil, fmt.Errorf("the signature section is at %d for %d bytes, which is outside the %d-byte file",
			start, stored, len(raw))
	}
	out := make([]byte, 0, len(raw)-int(stored))
	out = append(out, raw[:start]...)
	out = append(out, raw[start+stored:]...)
	return out, sec, nil
}

// findSection is the first section with that id.
func findSection(info *FileInfo, id uint8) *SectionInfo {
	if info == nil {
		return nil
	}
	for i := range info.Sections {
		if info.Sections[i].ID == id {
			return &info.Sections[i]
		}
	}
	return nil
}

// signatureOf reads the signature a file carries, or nil if it carries none.
func signatureOf(raw []byte) (*Signature, error) {
	f, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	snap, err := f.Load()
	if err != nil {
		return nil, err
	}
	return snap.Signature, nil
}

// findPublicKey looks for the key a signature was made with.
//
// A signature names a key, not a key's location, so the search is by the name beside
// the file first and then by the name the signature recorded. This is the honest
// arrangement: a file that carried its own public key would be signing itself, which
// proves only that whoever wrote it had a key. Verification here says "this file has
// not changed since it was signed by the holder of this key", which is a real
// property, and it is not "this file is trustworthy".
func findPublicKey(sig *Signature, dir string) (crypto.PublicKey, error) {
	names := []string{sig.Signer}
	// The signature also records the key file's own name, which for an x509
	// signature is a subject and for the others is a path; both are tried.
	if sig.KeyName != "" {
		names = append([]string{sig.KeyName}, names...)
	}
	var tried []string
	for _, name := range names {
		if name == "" || filepath.IsAbs(name) {
			continue
		}
		for _, d := range keySearchDirs(dir) {
			path := filepath.Join(d, name)
			tried = append(tried, path)
			if key, err := readPublicKey(path); err == nil {
				return key, nil
			}
		}
	}
	// Nothing found: say so plainly, and say where it looked, because a verifier
	// that reports "invalid" for a key it never found is worse than useless - it
	// teaches a person to ignore the answer.
	return nil, fmt.Errorf("no key to check this signature with; looked in %v. The signature is over a key this machine does not have, so it can be neither confirmed nor denied", tried)
}

// keySearchDirs is where a key is looked for: the directory the file is in, and
// the current one. Both are here because a file is usually signed and checked from
// the same shell, and that shell may be somewhere else entirely.
func keySearchDirs(dir string) []string {
	dirs := []string{dir}
	if dir != "." {
		dirs = append(dirs, ".")
	}
	return dirs
}

// readPublicKey accepts a PEM public key, a PEM certificate, or a PEM private key
// and returns the public part of whichever it is. A private key is accepted because
// the person who signed a file is very often the person verifying it, and making
// them export a public key first would be a step with no purpose behind it.
func readPublicKey(path string) (crypto.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rest := raw
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no PEM block")
		}
		switch block.Type {
		case "PUBLIC KEY":
			return x509.ParsePKIXPublicKey(block.Bytes)
		case "RSA PUBLIC KEY":
			return x509.ParsePKCS1PublicKey(block.Bytes)
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, err
			}
			return c.PublicKey, nil
		case "RSA PRIVATE KEY":
			k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			return &k.PublicKey, nil
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			s, ok := k.(crypto.Signer)
			if !ok {
				return nil, errors.New("that key cannot sign")
			}
			return s.Public(), nil
		}
	}
}

// rsaPublicOf is the RSA public key inside whatever public key was found, or an
// error that says what was found instead.
func rsaPublicOf(pub crypto.PublicKey) (*rsa.PublicKey, error) {
	if k, ok := pub.(*rsa.PublicKey); ok {
		return k, nil
	}
	return nil, fmt.Errorf("that key is a %T, not an RSA key", pub)
}
