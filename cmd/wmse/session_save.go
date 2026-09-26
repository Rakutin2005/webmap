package main

import (
	"fmt"
	"strings"

	"apimap/internal/wmse"
)

// ---------- source and save ----------

// archiveBrief says what the archive holds, and no longer assumes it is only
// reference files: `save` puts downloaded files in beside them, and a session
// that said "3 .ref files" over an archive holding a saved script would be
// describing something that is not there.
func archiveBrief(names []string) string {
	refs, saved := 0, 0
	for _, n := range names {
		if strings.HasSuffix(n, ".ref") {
			refs++
			continue
		}
		saved++
	}
	switch {
	case refs == 0:
		return plural(saved, "saved file")
	case saved == 0:
		return plural(refs, ".ref file")
	}
	return fmt.Sprintf("%d .ref, %s", refs, plural(saved, "saved file"))
}

// refNames is the reference sidecars alone, which is what `refs` and the reader's
// -refs section are about: a saved file is not a place a link was found, and
// listing it among the references would be describing an archive that does not
// exist.
func refNames(names []string) []string {
	var out []string
	for _, n := range names {
		if strings.HasSuffix(n, ".ref") {
			out = append(out, n)
		}
	}
	return out
}

// cmdSource hands a file back to the disk.
//
// The archive is asked first, because a file the scan already has is a file that
// does not need the network and cannot change under you: `source` on an archived
// path gives you exactly the bytes the scan recorded, and says so, so a repeated
// command on the same path is the same command. A path the archive does not hold
// is fetched from the server instead - and is deliberately *not* archived, because
// `source` writes to a path the person named and `save` is the command that grows
// the file. Making one do both would mean a read turning into a write of a file
// that may be somebody's scan of an hour's work.
func (s *session) cmdSource(args []string) {
	if len(args) < 2 {
		s.usage("source <path> <file-to-save>")
		return
	}
	full := s.resolve(args[0])
	dest := args[1]

	if data, err := s.archived(full); err == nil {
		n, err := writeOut(dest, data)
		if err != nil {
			fmt.Fprintf(s.out, "  %s\n", s.warn("cannot write "+dest+": "+err.Error()))
			return
		}
		fmt.Fprintf(s.out, "  %s %s\n", s.bold(wmse.ArchiveName(full)), s.dim(humanBytes(int64(n))+" from the archive"))
		fmt.Fprintf(s.out, "  %s\n", s.dim("-> "+dest))
		return
	}

	fmt.Fprintf(s.out, "  %s\n", s.dim("not in the archive; fetching "+full))
	data, ctype, err := s.download(full)
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
		return
	}
	n, err := writeOut(dest, data)
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn("cannot write "+dest+": "+err.Error()))
		return
	}
	fmt.Fprintf(s.out, "  %s %s\n", s.bold(s.field(full)), s.dim(humanBytes(int64(n))+" fetched"))
	if ctype != "" {
		fmt.Fprintf(s.out, "  %s\n", s.dim(ctype))
	}
	fmt.Fprintf(s.out, "  %s\n", s.dim("-> "+dest+" (not added to the archive; `save` does that)"))
}

// cmdSave downloads a file into the file's own archive, and rewrites the file.
//
// This is the one command in the session that changes the file it was opened on,
// and it says so before it does and reports the size after: an archive that grows
// without notice is how a 400 KiB scan becomes a 40 MiB one. The scan's findings
// are untouched - a saved file is a copy of what the server sent, kept so that
// `source` can answer for it later without the network.
func (s *session) cmdSave(args []string) {
	if len(args) == 0 || args[0] == "" {
		s.usage("save <path>")
		return
	}
	full := s.resolve(args[0])

	// The file being saved into has to exist and be writable, and it has to be one
	// this session has open: writing into a file that is not loaded would produce
	// a file the session cannot then read, which is the opposite of helpful.
	owner := s.fileHolding(full)
	if owner == nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn("this session has no file to save into"))
		fmt.Fprintf(s.out, "  %s\n", s.dim("`save` adds to a loaded scan's own archive; `extend` another file"))
		return
	}

	data, ctype, err := s.download(full)
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn(err.Error()))
		return
	}
	// Writing the file is what breaks a signature, so it is asked about before
	// anything is fetched rather than after: a person who has just been told their
	// scan is signed and that saving it makes the signature invalid should not have
	// to wait for a download to find out.
	if !s.warnBeforeEdit(owner) {
		return
	}
	name, err := owner.snap.AddArchiveFile(full, data)
	if err != nil {
		fmt.Fprintf(s.out, "  %s\n", s.warn("cannot add to the archive: "+err.Error()))
		return
	}
	// The archive grows the file, so the size is said before the write and the
	// write is atomic: an interrupted save leaves the scan as it was.
	before := owner.info.TotalBytes
	fmt.Fprintf(s.out, "  %s\n", s.dim("adding "+humanBytes(int64(len(data)))+" to "+owner.path))
	info, err := wmse.Write(owner.path, owner.snap, wmse.DefaultOptions())
	if err != nil {
		// The file is untouched - the write is atomic - but the in-memory archive
		// now holds a file the file on disk does not, so it is taken back out
		// rather than left to be written again by the next save.
		owner.snap.DropArchiveFile(name)
		fmt.Fprintf(s.out, "  %s\n", s.warn("cannot write "+owner.path+": "+err.Error()))
		fmt.Fprintf(s.out, "  %s\n", s.dim("the file is unchanged"))
		return
	}
	owner.info = info
	s.paths = nil
	s.refs = nil
	if s.files[0].snap == owner.snap {
		// The session's own view of the file it was opened on is that file, so
		// its merged data has to grow with it or `read` would describe a file
		// smaller than the one on disk.
		s.merged = owner.snap
		s.nodes, s.pages, s.pagePaths = nil, nil, nil
	}
	fmt.Fprintf(s.out, "  %s %s\n", s.bold(name), s.dim(humanBytes(int64(len(data)))))
	if ctype != "" {
		fmt.Fprintf(s.out, "  %s\n", s.dim(ctype))
	}
	// The delta is in bytes, not rounded: "2.0 KiB -> 2.0 KiB" is what a rounding
	// says about a file that grew, and the number a person wants after a save is
	// how much bigger it got.
	fmt.Fprintf(s.out, "  %s\n", s.dim(fmt.Sprintf("%s: %d B -> %d B (+%d B)",
		owner.path, before, info.TotalBytes, info.TotalBytes-before)))
	fmt.Fprintf(s.out, "  %s\n", s.dim("`source "+s.field(full)+" <file>` writes it back out"))
}

// archived returns the bytes the archive holds for a URL, or an error saying it
// holds nothing for it. The name is the one the writer would have used, derived
// here rather than remembered, so `save` then `source` cannot disagree about what
// a path was filed under.
func (s *session) archived(full string) ([]byte, error) {
	if len(s.merged.Archive) == 0 {
		return nil, fmt.Errorf("%s is not in the archive", full)
	}
	data, err := s.merged.ArchiveFile(wmse.ArchiveName(full))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// fileHolding is the loaded file a saved path belongs to: the one whose own
// archive would hold it, which is the one that was scanned that URL. With a single
// file it is that file, and with several it is the one that knows the path.
func (s *session) fileHolding(full string) *loadedFile {
	if len(s.files) == 1 {
		return &s.files[0]
	}
	for i := range s.files {
		for j := range s.files[i].snap.Links {
			if wmse.LinkKey(&s.files[i].snap.Links[j]) == full {
				return &s.files[i]
			}
		}
	}
	return nil
}

// ---------- source and save shared ----------
