package wmse

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// An encrypted file is a sealed file: a small header naming how it was sealed,
// followed by the sealed bytes. The original file is a whole number of sections
// and the sealed file shares none of them, so nothing can be read out of a sealed
// file without the key - including its size, its section count, and whether it was
// a scan at all.
//
// The header is authenticated, not encrypted. It has to be readable for the reader
// to know which algorithm to decrypt with, and it is fed to the cipher as
// additional data so that changing it - saying a file was sealed with a weaker key
// derivation than it was - makes decryption fail rather than succeed wrongly.

// sealMagic marks a sealed file, so one is not mistaken for a scan that happens to
// begin with bytes that look like a header.
const sealMagic = "WSEALED"

// sealVersion is the header layout version.
const sealVersion uint8 = 1

// ErrSealed is what a reader meets when a file has been encrypted. It is its own
// error because the answer to it is not "this file is broken": it is "this file is
// fine and you need the key", and a reader that said the first would be wrong in a
// way that costs somebody their scan.
var ErrSealed = errors.New("this file is encrypted")

// ErrWrongPassword is what a probe of a password-encrypted file reports when the
// password is not the right one. It says so rather than "authentication failed",
// because the person who typed it knows which of the two it is and a tool that
// makes them work it out from a cipher error is being unhelpful on purpose.
var ErrWrongPassword = errors.New("that is not the password")

// SealAlg is how a file was encrypted.
type SealAlg uint8

const (
	// SealNone is the absence of a seal.
	SealNone SealAlg = iota
	// SealPassword is AES-256-GCM under a key derived from a password with
	// PBKDF2-HMAC-SHA256.
	SealPassword
	// SealRSA is AES-256-GCM under a key sealed to an RSA public key with OAEP.
	// Whoever holds the private key can open it; nobody else can.
	SealRSA
	// SealAES is AES-256-GCM under a key read from a file, used as it stands.
	SealAES
)

// SealAlgName is what a person reads and writes.
func (a SealAlg) String() string {
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

// ParseSealAlg reads a sealing method the way a person writes it.
func ParseSealAlg(s string) (SealAlg, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "password", "pass":
		return SealPassword, nil
	case "rsa":
		return SealRSA, nil
	case "aes":
		return SealAES, nil
	case "none", "":
		return SealNone, nil
	}
	return SealNone, fmt.Errorf("unknown encryption method %q: password, rsa or aes", s)
}

// pbkdf2Iterations is the work factor for a password. It is high on purpose: it is
// the only thing standing between a stolen file and an offline guess, and the cost
// is paid once, by the person encrypting.
const pbkdf2Iterations = 600_000

// pbkdf2SaltLen and pbkdf2KeyLen are the salt and key sizes. The key is AES-256's.
const (
	pbkdf2SaltLen = 16
	pbkdf2KeyLen  = 32
	nonceLen      = 12
)

// SealFile encrypts a file in place.
//
// In place because "encrypt this file" means this file is encrypted, and because a
// copy left beside it would be the thing somebody sends to somebody else. The
// original is not recoverable without the key, which is the point and is said again
// by the caller before it happens.
func SealFile(path string, alg SealAlg, secret string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sealed, err := Seal(raw, alg, secret)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, sealed)
}

// Seal encrypts bytes with a password, an RSA public key, or a raw AES key.
//
// The three are kept apart rather than made one interface, because they answer
// different questions and a caller that mixed them up would be silently doing
// something else: a password can be wrong and retried, an RSA public key can be
// wrong only in the sense that the holder cannot open it, and an AES key is right
// or it is not.
func Seal(raw []byte, alg SealAlg, secret string) ([]byte, error) {
	if alg == SealNone {
		return nil, errors.New("no encryption method given")
	}
	if IsSealed(raw) {
		return nil, errors.New("this file is already encrypted")
	}
	h := sealHeader{Alg: alg}
	var aead cipher.AEAD
	var key []byte

	switch alg {
	case SealPassword:
		salt := make([]byte, pbkdf2SaltLen)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		var err error
		if key, err = pbkdf2.Key(sha256.New, secret, salt, pbkdf2Iterations, pbkdf2KeyLen); err != nil {
			return nil, err
		}
		h.Salt = salt
		h.Iterations = pbkdf2Iterations
		if aead, err = newGCM(key); err != nil {
			return nil, err
		}

	case SealAES:
		// A key file is used as it stands: 32 bytes is an AES-256 key, and
		// anything else is refused rather than stretched, because stretching a key
		// somebody chose by hand is a way of pretending a short key is a long one.
		var err error
		if key, err = os.ReadFile(secret); err != nil {
			return nil, fmt.Errorf("cannot read the key file: %w", err)
		}
		if len(key) != pbkdf2KeyLen {
			return nil, fmt.Errorf("the key file holds %d bytes; an AES-256 key is %d", len(key), pbkdf2KeyLen)
		}
		if aead, err = newGCM(key); err != nil {
			return nil, err
		}

	case SealRSA:
		pub, err := readPublicKey(secret)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(secret), err)
		}
		rsaPub, err := rsaPublicOf(pub)
		if err != nil {
			return nil, err
		}
		// The content key is random per file rather than derived from anything, so
		// two files sealed to the same public key are two unrelated files: one
		// leaked key does not open both.
		contentKey := make([]byte, pbkdf2KeyLen)
		if _, err := rand.Read(contentKey); err != nil {
			return nil, err
		}
		wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, rsaPub, contentKey, []byte(sealMagic))
		if err != nil {
			return nil, err
		}
		h.Wrapped = wrapped
		if aead, err = newGCM(contentKey); err != nil {
			return nil, err
		}
	}

	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	h.Nonce = nonce
	header := h.encode()
	// The header is authenticated rather than encrypted, so that a header edited to
	// name a weaker derivation is caught instead of obeyed.
	ct := aead.Seal(nil, nonce, raw, header)
	out := make([]byte, 0, len(header)+len(ct))
	out = append(out, header...)
	return append(out, ct...), nil
}

// sealHeader is what a sealed file starts with: enough to choose the algorithm and
// the work factor, and nothing that says what was sealed.
type sealHeader struct {
	Alg        SealAlg
	Salt       []byte
	Iterations int
	Nonce      []byte
	Wrapped    []byte
}

func (h sealHeader) encode() []byte {
	w := byteWriter{}
	w.rawString(sealMagic)
	w.byte(sealVersion)
	w.byte(uint8(h.Alg))
	w.uvarint(uint64(h.Iterations))
	w.uvarint(uint64(len(h.Salt)))
	w.bytes(h.Salt)
	w.uvarint(uint64(len(h.Nonce)))
	w.bytes(h.Nonce)
	w.uvarint(uint64(len(h.Wrapped)))
	w.bytes(h.Wrapped)
	return w.buf
}

func decodeSealHeader(raw []byte) (sealHeader, []byte, error) {
	var h sealHeader
	r := &byteReader{buf: raw}
	magic, err := r.rawString()
	if err != nil {
		return h, nil, ErrSealed
	}
	if magic != sealMagic {
		return h, nil, errors.New("not a sealed file")
	}
	version, err := r.byteAt()
	if err != nil {
		return h, nil, ErrSealed
	}
	if version != sealVersion {
		return h, nil, fmt.Errorf("this file was sealed with layout version %d, which this reader does not know", version)
	}
	alg, err := r.byteAt()
	if err != nil {
		return h, nil, ErrSealed
	}
	h.Alg = SealAlg(alg)
	if h.Iterations, err = r.count(1 << 20); err != nil {
		return h, nil, ErrSealed
	}
	if h.Salt, err = take(r); err != nil {
		return h, nil, ErrSealed
	}
	if h.Nonce, err = take(r); err != nil {
		return h, nil, ErrSealed
	}
	if h.Wrapped, err = take(r); err != nil {
		return h, nil, ErrSealed
	}
	return h, raw[r.pos:], nil
}

// IsSealed reports whether a file's bytes are an encrypted file. It reads only the
// magic, so it can be asked about a file nobody has the key for - which is the
// question a reader has to answer before it can ask for anything else.
func IsSealed(raw []byte) bool {
	magic, err := (&byteReader{buf: raw}).rawString()
	return err == nil && magic == sealMagic
}

// SealedState says whether a file on disk is sealed, and how. It exists so a
// command that has a file rather than its bytes can ask the same question
// openSealed answers, without holding a whole scan in memory to do it.
func SealedState(path string) (bool, SealAlg) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, SealNone
	}
	if !IsSealed(raw) {
		return false, SealNone
	}
	alg, err := SealMethod(raw)
	if err != nil {
		return true, SealNone
	}
	return true, alg
}

// SealMethod names how a sealed file was encrypted, without the key. A reader can
// say "this was encrypted with a password" and therefore that it has to ask for one,
// which is the first thing it needs to know.
func SealMethod(raw []byte) (SealAlg, error) {
	h, _, err := decodeSealHeader(raw)
	if err != nil {
		return SealNone, err
	}
	return h.Alg, nil
}

// Unseal decrypts a sealed file.
//
// A wrong password is reported as a wrong password. AEAD cannot tell a wrong key
// from a tampered file, and every implementation of this has to pick what to say
// about the ambiguity; saying "the password is wrong" is the answer that is true
// every time somebody is prompted, and the tamper case is caught anyway because a
// tampered file does not decrypt to something that parses.
func Unseal(raw []byte, secret string) ([]byte, error) {
	h, body, err := decodeSealHeader(raw)
	if err != nil {
		return nil, err
	}
	header := h.encode()

	var key []byte
	switch h.Alg {
	case SealPassword:
		key, err = pbkdf2.Key(sha256.New, secret, h.Salt, h.Iterations, pbkdf2KeyLen)
		if err != nil {
			return nil, err
		}
	case SealAES:
		key, err = os.ReadFile(secret)
		if err != nil {
			return nil, fmt.Errorf("cannot read the key file: %w", err)
		}
	case SealRSA:
		priv, err := readPrivateKey(secret)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(secret), err)
		}
		rsaKey, ok := priv.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s is not an RSA private key", filepath.Base(secret))
		}
		var derr error
		if key, derr = rsa.DecryptOAEP(sha256.New(), rand.Reader, rsaKey, h.Wrapped, []byte(sealMagic)); derr != nil {
			return nil, errors.New("that private key did not open this file")
		}
	default:
		return nil, fmt.Errorf("this file names an encryption method this reader does not know: %d", h.Alg)
	}

	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, h.Nonce, body, header)
	if err != nil {
		if h.Alg == SealPassword {
			return nil, ErrWrongPassword
		}
		return nil, errors.New("that key did not open this file")
	}
	return plain, nil
}

// readPrivateKey is the private half of readPublicKey's tolerance, for the file that
// has to be able to open a sealed one.
func readPrivateKey(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	priv, _, err := parsePrivateKey(raw)
	return priv, err
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// take reads a length-prefixed run of bytes.
func take(r *byteReader) ([]byte, error) {
	n, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(r.buf)-r.pos) {
		return nil, ErrTruncated
	}
	out := r.buf[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return out, nil
}
