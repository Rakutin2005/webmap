package contract

import "strings"

// maxArgs is how wide an argument list may be in a report column before it is cut.
const maxArgs = 70

// CleanArgs turns the argument text a JavaScript call site carries - which is
// whatever the parser read out of the source, so it can be ragged, wrapped, or
// empty - into the one-line form a report prints after "args: ". It returns an
// empty string when what it found is too short to be an argument list at all: a
// bare "(" or "[]" is punctuation, not an argument, and printing "args: []" for
// every endpoint in a site would be noise dressed as information.
//
// It lives here rather than in the command that prints it because the scan and
// the static reader print the same rows, and two copies of a display rule drift
// within a release or two.
func CleanArgs(args string) string {
	args = strings.TrimSpace(args)
	args = strings.TrimLeft(args, ",")
	args = strings.TrimSpace(args)
	args = strings.ReplaceAll(args, "\n", " ")
	args = strings.ReplaceAll(args, "\r", "")
	for strings.Contains(args, "  ") {
		args = strings.ReplaceAll(args, "  ", " ")
	}
	if len(args) < 2 || (len(args) < 5 && !strings.ContainsAny(args, "{[/")) {
		return ""
	}
	if len(args) <= maxArgs {
		return args
	}
	return args[:maxArgs-3] + "..."
}
