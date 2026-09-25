package linker

import (
	"fmt"
	"sort"
	"strings"
)

// maxParamValues is how many distinct values of one parameter name a row will
// list. A parameter that took fifty values is still a parameter, and the first
// few are enough to see what shape it has; the rest are in the file.
const maxParamValues = 5

// FormatParamVariants renders the queries a link was requested with as the
// annotation both reports print after a row: "page=2; q=term", with repeated
// values of one name folded into a list and more than a handful cut short.
//
// It is here, on the type it renders, because the scan and the static reader
// print the same rows: two copies of a formatting rule is a rule that will
// disagree between them within a release or two, and the disagreement would look
// like a different scan rather than a different renderer.
func FormatParamVariants(variants []ParamVariant) string {
	if len(variants) == 0 {
		return ""
	}
	// name -> the values it was seen with, in the order and multiplicity that
	// does not matter because the output is sorted.
	paramValues := make(map[string]map[string]bool)
	seenQueries := make(map[string]bool)
	for _, v := range variants {
		q := v.Query
		if seenQueries[q] {
			continue
		}
		seenQueries[q] = true
		q = strings.TrimPrefix(q, "?")
		pairs := strings.Split(q, "&")
		for _, pair := range pairs {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) == 2 && kv[0] != "" {
				if paramValues[kv[0]] == nil {
					paramValues[kv[0]] = make(map[string]bool)
				}
				if kv[1] != "" {
					paramValues[kv[0]][kv[1]] = true
				}
			}
		}
	}
	var parts []string
	for name, vals := range paramValues {
		if len(vals) == 0 {
			// A name with no value seen: "?debug" is a parameter, and the
			// value is exactly the thing that was never observed.
			parts = append(parts, name)
			continue
		}
		valList := make([]string, 0, len(vals))
		for v := range vals {
			valList = append(valList, v)
		}
		sort.Strings(valList)
		if len(valList) <= maxParamValues {
			parts = append(parts, fmt.Sprintf("%s=%s", name, strings.Join(valList, ",")))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%s+...", name, strings.Join(valList[:maxParamValues], ",")))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
