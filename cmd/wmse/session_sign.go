package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"apimap/internal/wmse"
)

// ---------- sign and encrypt ----------

// cmdSign signs the file the session has open, or removes its signature.
//
// Signing is a whole-file act, so it goes through the same file the session was
// opened on and says so before it does. It does not change the findings: a
// signature is a statement about the file, not a fact about the site.
func (s *session) cmdSign(args []string) {
	if len(args) == 0 || args[0] == "" {
		s.usage("sign <rsa|x509|gpg|none> <path-to-key>")
		return
	}
	alg, err := wmse.ParseSignAlg(args[0])
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
		return
	}
	if len(args) < 2 {
		s.usage("sign <rsa|x509|gpg|none> <path-to-key>")
		return
	}
	owner := &s.files[0]
	path := owner.path
	key := args[1]

	// A file that is already encrypted cannot be signed: the signature would be
	// over bytes nobody can see, and verifying it would need the file decrypted
	// first - which is a rule worth saying out loud rather than discovering.
	if sealed, _ := wmse.SealedState(path); sealed {
		fmt.Fprintf(s.out, "  %s\n", s.warn("this file is encrypted, so there is nothing in it to sign"))
		fmt.Fprintf(s.out, "  %s\n", s.dim("decrypt it first, sign what comes out, and seal it again"))
		return
	}

	if alg == wmse.SignNone {
		if err := s.clearSignature(owner); err != nil {
			fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
			return
		}
		fmt.Fprintf(s.out, "  %s\n", s.bold("signature removed"))
		fmt.Fprintf(s.out, "  %s\n", s.dim(path+" now says nothing about itself"))
		return
	}

	// A file that is already signed, signed again, is a normal thing to do - after
	// a save, say. It is said, because the old signature is being replaced and
	// somebody may be holding a copy that verified yesterday.
	if owner.snap.Signature != nil {
		fmt.Fprintf(s.out, "  %s\n", s.dim("this file is already signed; the signature is being replaced"))
	}
	sig, err := wmse.SignFile(path, alg, key)
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
		return
	}
	// The session's own copy of the file has to learn about the signature, or `info`
	// would describe the file as it was before the command that just changed it.
	if err := s.reload(owner); err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn("signed, but the session could not re-read it: "+err.Error()))
		return
	}
	fmt.Fprintf(s.out, "  %s %s\n", s.bold("signed"), s.dim(sig.Alg.String()))
	fmt.Fprintf(s.out, "  %s\n", s.dim("by "+sig.Signer))
	fmt.Fprintf(s.out, "  %s\n", s.dim("key "+sig.KeyID+" ("+sig.KeyName+")"))
	fmt.Fprintf(s.out, "  %s\n", s.dim("the signature covers the file with this section taken out"))
}

// clearSignature removes a signature by rewriting the file without the section.
func (s *session) clearSignature(owner *loadedFile) error {
	if owner.snap.Signature == nil {
		return errors.New("this file is not signed")
	}
	owner.snap.Signature = nil
	if _, err := wmse.Write(owner.path, owner.snap, wmse.DefaultOptions()); err != nil {
		return err
	}
	return s.reload(owner)
}

// cmdEncrypt seals the file the session has open.
//
// It is the one command that takes a file away from the person who opened it: after
// this, the file on disk is ciphertext and the session is holding the only readable
// copy. So it says that, and asks, before it does - the same warning a signed file
// gets before an edit, and for a sharper reason.
func (s *session) cmdEncrypt(args []string) {
	if len(args) < 2 {
		s.usage("encrypt <password|rsa|aes> <password-or-path-to-key>")
		return
	}
	alg, err := wmse.ParseSealAlg(args[0])
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
		return
	}
	if alg == wmse.SealNone {
		fmt.Fprintf(s.out, "  %s\n", s.warn("no encryption method given: password, rsa or aes"))
		return
	}
	owner := &s.files[0]
	path := owner.path
	secret := args[1]

	// A password typed as an argument is in the shell's history and in the process
	// list, so it is said plainly - once, here, where it can still be changed.
	if alg == wmse.SealPassword {
		fmt.Fprintf(s.out, "  %s\n", s.warn("a password given on the command line is in your shell history and in the process list"))
		fmt.Fprintf(s.out, "  %s\n", s.dim("`encrypt password` with no password asks for it instead, and keeps it out of both"))
	}

	fmt.Fprintf(s.out, "  %s\n", s.dim(fmt.Sprintf("encrypting %s with %s; after this the file on disk cannot be read without it",
		path, alg)))
	if !s.confirm("continue? [y/N] ") {
		fmt.Fprintf(s.out, "  %s\n", s.dim("not encrypted"))
		return
	}
	if err := wmse.SealFile(path, alg, secret); err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
		return
	}
	fmt.Fprintf(s.out, "  %s %s\n", s.bold("encrypted"), s.dim(alg.String()))
	fmt.Fprintf(s.out, "  %s\n", s.dim(fmt.Sprintf("%s: the file on disk is now ciphertext", path)))
	fmt.Fprintf(s.out, "  %s\n", s.dim("this session still has the file in memory; open it with -i to have it decrypted again"))
}

// confirm asks a question and waits for a yes.
//
// A session with no terminal to ask on answers no: a command that changes a file
// irreversibly must not be run by a script that never saw the question, and
// "nobody was asked" is not consent.
//
// The answer is read with the same line editor the session types commands with,
// rather than by reading the terminal again. That is not a preference: a second
// reader of the same terminal is a second thing racing the first for its input, and
// the question is the part that must not lose. It also means the answer is edited
// and echoed like everything else, so a mistyped "n" is visible before it refuses.
func (s *session) confirm(question string) bool {
	if !s.interactive || s.keys == nil {
		return false
	}
	ed := &lineEditor{
		src: s.keys,
		out: s.out,
		// No history: a question is typed once and answered once, and the up arrow
		// must not recall answers as if they were commands.
		hist:   nil,
		prompt: func() string { return "  " + question },
	}
	answer, err := ed.read()
	if err != nil {
		// End of input at a question is a no. The alternative is a session that
		// cannot be closed while a question is outstanding.
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}

// warnBeforeEdit says what writing a signed file does to its signature, and asks.
//
// This is the point of a signature. A file that was signed and then changed is not
// evidence of anything the signer vouched for, and the person who signs a scan is
// often not the person looking at it later. So the write stops and asks, rather
// than happening and leaving a signature that no longer means what it says.
//
// The answer is remembered for the rest of the session: a person who has been told
// once and said yes does not need to be told again for every save.
func (s *session) warnBeforeEdit(owner *loadedFile) bool {
	if owner.snap.Signature == nil {
		return true
	}
	if s.editWarned {
		return true
	}
	// A signature that does not verify is a different conversation, and it was had
	// when the file was opened. What is left to say here is that it is about to get
	// worse.
	fmt.Fprintf(s.out, "  %s\n", s.warn("this file is signed, and writing it makes the signature invalid"))
	if owner.snap.Signature.Signer != "" {
		fmt.Fprintf(s.out, "  %s\n", s.dim("signed by "+owner.snap.Signature.Signer+
			" on "+whenOf(owner.snap.Signature)))
	}
	fmt.Fprintf(s.out, "  %s\n", s.dim("the signature is not re-made, because only the signer can do that"))
	if !s.confirm("write it anyway? [y/N] ") {
		fmt.Fprintf(s.out, "  %s\n", s.dim("not written"))
		return false
	}
	s.editWarned = true
	return true
}

// whenOf is when a signature was made, in words.
func whenOf(sig *wmse.Signature) string {
	if sig.When.IsZero() {
		return "an unrecorded date"
	}
	return sig.When.UTC().Format("2006-01-02 15:04 MST")
}

// ---------- opening a file that is sealed ----------

// openSealed reads a file that is encrypted, asking for whatever it takes.
//
// A password is asked for and probed, because a password the person typed is the
// only evidence there is that it is right, and a file that does not open is a file
// they need to be told about rather than left waiting on. A key is taken from -i,
// because a key is a file and a file needs no asking.
func openSealed(path string, keyPath string, in *os.File, out *os.File) (*wmse.Snapshot, *wmse.FileInfo, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	alg, err := wmse.SealMethod(raw)
	if err != nil {
		return nil, nil, err
	}
	secret := keyPath
	if alg == wmse.SealPassword {
		if keyPath != "" {
			return nil, nil, errors.New("this file was encrypted with a password, which cannot be read from a key file; type it when asked")
		}
		secret, err = askPassword(out, in)
		if err != nil {
			return nil, nil, err
		}
	} else if keyPath == "" {
		return nil, nil, fmt.Errorf("this file was encrypted with %s, so it needs a key: wmse select %s -i <key>", alg, path)
	}
	plain, err := wmse.Unseal(raw, secret)
	if err != nil {
		return nil, nil, err
	}
	// The decrypted bytes are parsed exactly as a file that was never encrypted, so
	// a sealed file is not a second format once it is open.
	f, err := wmse.Parse(plain)
	if err != nil {
		return nil, nil, fmt.Errorf("the file opened but is not a scan: %w", err)
	}
	snap, err := f.Load()
	if err != nil {
		return nil, nil, err
	}
	return snap, f.Info(), nil
}

// askPassword asks for a password without echoing it.
//
// A terminal is put out of its echo for the duration rather than the password being
// read and then ignored: a password that appears on the screen is a password in a
// screenshot, and asking for one that is going to be hidden makes it obvious when
// it is not.
func askPassword(out *os.File, in *os.File) (string, error) {
	if in == nil {
		return "", errors.New("a password is needed and there is no terminal to ask on")
	}
	fmt.Fprint(out, "  password: ")
	restore, err := hideEcho(in)
	if err != nil {
		// No echo to turn off is not a reason to refuse: the password is still
		// asked for, and saying so is better than a failure.
		fmt.Fprintf(out, "  %s\n", dimText("(this terminal will show what you type)"))
	}
	line, err := readLine(in)
	if restore != nil {
		restore()
	}
	fmt.Fprintln(out)
	if err != nil && line == "" {
		return "", err
	}
	if line == "" {
		return "", errors.New("no password given")
	}
	return line, nil
}

// readLine reads one line from a file without buffering the rest of it, so that the
// session's own reader still gets the input after the password.
func readLine(f *os.File) (string, error) {
	var line []byte
	var one [1]byte
	for {
		n, err := f.Read(one[:])
		if n == 1 {
			if one[0] == '\n' {
				return strings.TrimRight(string(line), "\r"), nil
			}
			line = append(line, one[0])
			continue
		}
		if err != nil {
			return strings.TrimRight(string(line), "\r"), err
		}
	}
}

func dimText(s string) string { return "\033[2m" + s + "\033[0m" }
