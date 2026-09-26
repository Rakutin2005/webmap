package wmse

import (
	"fmt"
	"time"
)

// The signature section's own encoding. It is deliberately dull: a magic, a
// version, the algorithm, and the fields that let a reader say who signed the file
// without having the key. The signature itself comes last and is length-prefixed,
// so a field can be added later without a reader having to guess where the
// signature begins.
//
// The strings are written inline rather than interned into the file's string table.
// They are read one at a time, out of a file nobody is going to grep, and putting
// "signed by" into the same table as every URL string would make the table a place
// to look for the signer - which is the opposite of what a section nobody asked to
// read should be.

// sigMagic marks a signature block, so a section filled with something else is
// recognised as wrong rather than read as a signature.
const sigMagic = "WSIG"

// sigVersion is the layout version. A reader that does not know this version says so
// instead of guessing at the fields.
const sigVersion uint8 = 1

// writeSignature emits the signature section, or nothing at all for a file that was
// not signed.
//
// A file with no signature has no section rather than an empty one, because "this
// file makes no claim" and "this file has a claim that is empty" are different
// answers, and a reader has to be able to tell them apart.
func (st *encoderState) writeSignature() {
	if st.snap.Signature == nil {
		return
	}
	sig := st.snap.Signature
	w := byteWriter{}
	w.rawString(sigMagic)
	w.byte(sigVersion)
	w.byte(uint8(sig.Alg))
	w.intvarint(sig.When.UnixNano())
	w.rawString(sig.Signer)
	w.rawString(sig.KeyID)
	w.rawString(sig.KeyName)
	w.uvarint(uint64(len(sig.Sig)))
	w.bytes(sig.Sig)
	st.put(SecSignature, w.buf)
}

// readSignature reads the signature section. A section that does not begin with the
// magic is a corrupt file rather than an unsigned one, and says so: a reader that
// treated unreadable bytes as "no signature" would report a tampered file as
// perfectly ordinary, which is the one mistake this whole mechanism exists to
// prevent.
func readSignature(r *byteReader, _ *decoder, snap *Snapshot) error {
	magic, err := r.rawString()
	if err != nil {
		return err
	}
	if magic != sigMagic {
		return fmt.Errorf("%w: the signature section does not begin with %s", ErrBadSection, sigMagic)
	}
	version, err := r.byteAt()
	if err != nil {
		return err
	}
	if version != sigVersion {
		return fmt.Errorf("the signature section is version %d, which this reader does not know", version)
	}
	alg, err := r.byteAt()
	if err != nil {
		return err
	}
	sig := &Signature{Alg: SignAlg(alg)}
	ns, err := r.intvarint()
	if err != nil {
		return err
	}
	sig.When = time.Unix(0, ns).UTC()
	if sig.Signer, err = r.rawString(); err != nil {
		return err
	}
	if sig.KeyID, err = r.rawString(); err != nil {
		return err
	}
	if sig.KeyName, err = r.rawString(); err != nil {
		return err
	}
	n, err := r.uvarint()
	if err != nil {
		return err
	}
	if n > uint64(len(r.buf)-r.pos) {
		return ErrTruncated
	}
	body := r.buf[r.pos : r.pos+int(n)]
	r.pos += int(n)
	// The signature is copied rather than referenced, so that a decoded snapshot
	// does not hold the whole section's buffer alive for the length of one
	// signature.
	sig.Sig = append([]byte(nil), body...)
	snap.Signature = sig
	return nil
}
