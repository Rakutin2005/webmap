package wmse

import (
	"strconv"

	"apimap/internal/archive"
)

// AddArchiveFile puts a downloaded file into the snapshot's archive, under the
// name its URL maps to, and returns that name.
//
// The archive is rebuilt rather than appended to, because it is a tar: a tar
// cannot have a member added to the end and still be a tar that lists itself
// correctly. Rebuilding from the entries already there plus the new one is the
// only way to keep the manifest honest, and the manifest is what makes the
// archive readable by anything but the code that wrote it.
//
// Storing the bytes is what makes `source` work offline afterwards, and it is
// also what makes the file grow: a saved file is a copy of what the server sent,
// kept in the scan, and a scan that saves a large asset carries that weight from
// then on. The caller says so before it does.
func (s *Snapshot) AddArchiveFile(rawURL string, data []byte) (string, error) {
	name := ArchiveName(rawURL)
	entries, err := s.ArchiveEntries()
	if err != nil {
		return "", err
	}
	// A second save of the same URL replaces what was there, rather than
	// producing two entries with one name between them - which is a tar that
	// extracts one of them and leaves the other behind.
	kept := entries[:0]
	for _, e := range entries {
		if e.Name != name {
			kept = append(kept, e)
		}
	}
	entries = append(kept, archive.Entry{Name: name, Data: data})

	raw, names, err := archive.Build(entries)
	if err != nil {
		return "", err
	}
	s.Archive = raw
	s.ArchiveNames = names
	s.Meta[MetaArchiveFiles] = strconv.Itoa(len(names))
	return name, nil
}

// DropArchiveFile removes an entry from the archive by name, rebuilding it.
//
// It exists for the case where a file was added to the archive in memory and the
// write of the file it belonged to then failed: the session's view would hold a
// file the file on disk does not, and the next save would write both. Taking it
// back out leaves the two in step, which is the whole point of the atomic write.
func (s *Snapshot) DropArchiveFile(name string) error {
	entries, err := s.ArchiveEntries()
	if err != nil {
		return err
	}
	kept := make([]archive.Entry, 0, len(entries))
	for _, e := range entries {
		if e.Name != name {
			kept = append(kept, e)
		}
	}
	raw, names, err := archive.Build(kept)
	if err != nil {
		return err
	}
	s.Archive = raw
	s.ArchiveNames = names
	s.Meta[MetaArchiveFiles] = strconv.Itoa(len(names))
	return nil
}

// ArchiveEntries unpacks the archive into its entries, which is what rebuilding
// it needs. An archive that cannot be unpacked is an error rather than something
// to write over: replacing it would throw away files the scan had already saved.
func (s *Snapshot) ArchiveEntries() ([]archive.Entry, error) {
	if len(s.Archive) == 0 {
		return nil, nil
	}
	return archive.Entries(s.Archive)
}
