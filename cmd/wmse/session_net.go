package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"apimap/internal/fetcher"
)

// The session fetches for a living now. `source` and `save` are about getting a
// file the scan saw, `req` is about asking the server something the scan did not,
// and `continue` is about scanning further. All of them go out over the network
// and, two of them, to the disk, so this file is where the shared parts live: how
// a path becomes a URL to fetch, and how the session's fetcher is built from what
// the file recorded about the scan that made it.

// sessionFetcher builds a fetcher for the session's target, carrying over the one
// setting that would otherwise make every request fail: whether the scan skipped
// TLS verification.
//
// That setting is recovered from the command line the file records rather than
// asked for, because a file scanned with -k against a certificate nobody trusts is
// a file whose every request would otherwise be refused, and the person who
// scanned it already answered the question. A file that records no command is
// fetched the strict way, which is the safe way to be wrong in.
func (s *session) sessionFetcher() (*fetcher.Fetcher, error) {
	base := s.target
	if base == "" {
		base = s.pos
	}
	if base == "" {
		return nil, fmt.Errorf("this session has no target to fetch from")
	}
	f, err := fetcher.New(base, recordedHas(s.merged.Meta["command"], "-k"))
	if err != nil {
		return nil, err
	}
	// The recorded scan may have been given headers or cookies of its own, and a
	// request that omits them is a request to a page that answers differently.
	// They are applied to the target's own host only: the file may also name a
	// third party, and a token must not follow one there.
	f.SetHeaderScope(s.inScope)
	return f, nil
}

// inScope says whether user-supplied headers may be sent to a host: the target's
// own, and any domain the file was told to follow.
func (s *session) inScope(host string) bool {
	return sameHost("https://"+host, s.target)
}

// recordedHas reports whether a recorded command line used a flag. It is how the
// one setting a fetch cannot do without - -k - is recovered from the file.
func recordedHas(command, flag string) bool {
	for _, f := range readersFlagsFrom(command) {
		if f == flag {
			return true
		}
	}
	return false
}

// download fetches a URL and returns its bytes with the response's content type.
//
// The status is checked here rather than left to the caller, because a body that
// is an error page is not the file that was asked for: saving "404 Not Found" to
// disk under the name of a script would be worse than refusing, and it would
// refuse at the point of use rather than here.
func (s *session) download(full string) ([]byte, string, error) {
	f, err := s.sessionFetcher()
	if err != nil {
		return nil, "", err
	}
	res, err := f.Fetch(full)
	if err != nil {
		return nil, "", err
	}
	if res.Status < 200 || res.Status > 299 {
		// The code first: "answered Not Found" is a phrase, and 404 is the thing a
		// person would look up, compare against a log, or paste into a note.
		return nil, "", fmt.Errorf("%s answered %d %s", full, res.Status, http.StatusText(res.Status))
	}
	return []byte(res.Body), res.Headers.Get("Content-Type"), nil
}

// writeOut writes bytes to a path, refusing before it starts if it cannot.
//
// A file is written through a temporary name in the same directory and renamed
// over the target, so a download that fails part way through leaves no half file
// where a whole one used to be. A tool that replaces somebody's notes with 400
// bytes of a proxy's error page is a tool nobody runs twice.
func writeOut(path string, data []byte) (int, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".part*")
	if err != nil {
		return 0, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(name, path); err != nil {
		return 0, err
	}
	return len(data), nil
}
