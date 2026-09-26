package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ---------- req ----------

// cmdRequest runs a request with curl, with the arguments the person typed passed
// through to it untouched.
//
// curl is run rather than imitated. The arguments a person writes here are curl's
// own - `-H "authorization: Bearer ..."`, `-X POST`, `-d @body.json` - and a Go
// implementation of that subset would agree with curl on the common cases and
// disagree on the rest, which is the worst of both: the request that goes out
// would not be the request that was asked for, and nothing would say so. Running
// the real thing means the exchange is curl's, in every case.
//
// The arguments go to curl as its own argv, with no shell between, so a value
// containing a space, a quote or a semicolon is one argument and not a command.
// The path is resolved against where the session is standing, and placed after
// the arguments, which is where curl takes a URL.
func (s *session) cmdRequest(args []string) {
	if len(args) == 0 || args[0] == "" {
		s.usage("req <path> [curl arguments]")
		return
	}
	full := s.resolve(args[0])
	curlArgs := append(append([]string{}, args[1:]...), full)

	// The request is shown before it is made. This is the one command in the
	// session that can change something on the server, and a request that is
	// about to be sent ought to be readable before it is sent, not after.
	fmt.Fprintf(s.out, "  %s\n", s.dim("curl "+strings.Join(quoteArgs(curlArgs), " ")))

	cmd := exec.Command("curl", curlArgs...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	started := time.Now()
	runErr := cmd.Run()
	took := time.Since(started).Round(time.Millisecond)

	if msg := strings.TrimRight(errOut.String(), "\n"); msg != "" {
		for _, l := range strings.Split(msg, "\n") {
			fmt.Fprintf(s.out, "  %s\n", s.dim(l))
		}
	}
	body := out.String()
	if body != "" {
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		fmt.Fprint(s.out, body)
	}
	switch {
	case runErr == nil:
		fmt.Fprintf(s.out, "  %s\n", s.dim(fmt.Sprintf("ok in %s, %s", took, humanBytes(int64(out.Len())))))
	default:
		// curl's own status is the useful part: 22 for an HTTP error, 6 for a
		// host that would not resolve, 7 for a refused connection. It is named
		// rather than printed as a number, because "exit status 22" tells a
		// reader nothing and "HTTP error" does.
		fmt.Fprintf(s.out, "  %s\n", s.warn(curlFailure(runErr)))
		fmt.Fprintf(s.out, "  %s\n", s.dim(fmt.Sprintf("after %s", took)))
	}
}

// curlFailure names what curl's exit status means, so the session says what
// happened rather than printing a number the reader has to look up.
func curlFailure(err error) string {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err.Error()
	}
	switch ee.ExitCode() {
	case 6:
		return "the host could not be resolved"
	case 7:
		return "the connection was refused"
	case 22:
		return "the server answered with an HTTP error"
	case 28:
		return "the request timed out"
	case 35:
		return "the TLS handshake failed (the scan may have needed -k)"
	case 47:
		return "too many redirects"
	case 52:
		return "the server sent nothing back"
	}
	return fmt.Sprintf("curl exited with status %d", ee.ExitCode())
}

// quoteArgs puts an argument back the way a person would have to type it, so the
// request being shown is the request that will run. A value that already contains
// a space is quoted; one that does not is left alone, because quoting everything
// makes the common case harder to read than the case that needs it.
func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'\\$`;|&<>()") {
			out[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a) + `"`
			continue
		}
		out[i] = a
	}
	return out
}
