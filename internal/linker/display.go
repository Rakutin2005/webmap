package linker

import (
	"fmt"
	"sort"
	"strings"
)

// FoldParamLinks prepares a discovered link set for the link table.
//
// A crawl finds one link per href, so /item?id=1 and /item?id=2 arrive as two
// records and a table of them reads as a site with twice the pages it has. The
// table therefore shows one row per base URL and lists the queries as that row's
// parameters, which is the form in which a person can use the result: the row is
// the endpoint, the annotation is what it was called with.
//
// The fold keeps the class in the key, because two records of one base URL that
// differ in class are not the same finding: a WAF response to /item is not the
// item. It keeps the record of a bare base URL if one exists, because that is
// the request with no parameters and it is the one a reader can copy.
//
// It lives on the type it renders because the scan and the static reader print
// the same table: two copies of a folding rule is a table that disagrees with
// the run that produced the file.
func FoldParamLinks(links []Link) []Link {
	type grouped struct {
		link     *Link
		variants []ParamVariant
		seenBase bool
	}
	groups := make(map[string]*grouped)
	result := make([]Link, 0, len(links))

	for _, l := range links {
		resolved := l.Resolved
		if resolved == "" {
			resolved = l.HREF
		}
		base, rawQuery := SplitQuery(resolved)
		key := fmt.Sprintf("%s|%d", base, l.Class)

		g, seen := groups[key]
		if rawQuery == "" {
			// A record of the bare URL is the row: the request as it
			// stands is the one a reader can copy, so it takes over the
			// group's identity whether it arrives before or after the
			// queried records. Which is first is not a fact about the
			// site - a crawl finds links in whatever order its workers
			// finish in - so a fold that depended on it would print a
			// different table for the same scan.
			if seen {
				g.seenBase = true
				g.link.HREF = l.HREF
				g.link.Resolved = l.Resolved
				g.link.SourceURL = l.SourceURL
				g.link.Tag = l.Tag
				g.link.Domain = l.Domain
				g.link.APIDetails = l.APIDetails
			} else {
				parent := l
				groups[key] = &grouped{link: &parent, seenBase: true}
			}
			continue
		}

		if !seen {
			parent := l
			parent.HREF = DropQuery(l.HREF)
			parent.Resolved = base
			parent.HasParams = false
			groups[key] = &grouped{link: &parent}
			g = groups[key]
		}
		g.variants = append(g.variants, ParamVariant{Query: rawQuery})
		if !g.seenBase {
			g.link.ParamVariants = g.variants
		}
	}

	for _, g := range groups {
		g.link.ParamVariants = g.variants
		result = append(result, *g.link)
	}

	return result
}

// SplitQuery splits a URL into its base and its query, the query keeping the "?"
// that marked it. A URL with no query has an empty second half, which is what
// callers test to tell a bare URL from one that was called with parameters.
func SplitQuery(rawURL string) (base string, query string) {
	idx := strings.Index(rawURL, "?")
	if idx < 0 {
		return rawURL, ""
	}
	rest := rawURL[idx+1:]
	if rest == "" {
		return rawURL, ""
	}
	return rawURL[:idx], "?" + rest
}

// DropQuery is SplitQuery's base half on its own: the URL with its query
// removed, which is what a folded row shows as the thing it stands for.
func DropQuery(rawURL string) string {
	base, _ := SplitQuery(rawURL)
	return base
}

// MergeAPIDetails collapses records that share an href into one, keeping every
// method each record was seen with. Two records of one URL are the same URL, and
// printing it twice would make the endpoint count read as a link count.
func MergeAPIDetails(links []Link) []Link {
	byHREF := make(map[string]*Link)
	result := make([]Link, 0, len(links))
	for i := range links {
		if existing, ok := byHREF[links[i].HREF]; ok {
			if len(links[i].APIDetails) > 0 {
				existing.APIDetails = append(existing.APIDetails, links[i].APIDetails...)
			}
			if len(links[i].ParamVariants) > 0 {
				existing.ParamVariants = append(existing.ParamVariants, links[i].ParamVariants...)
			}
		} else {
			byHREF[links[i].HREF] = &links[i]
			result = append(result, links[i])
		}
	}
	return result
}

// SortForDisplay orders a link set the way the report prints it: by class, then
// category, then domain, then the URL itself. The order is a reading order - the
// things of one kind stay together, and within a kind the alphabetic one is
// next - rather than a ranking, so the comparison has to be a total one and
// cannot stop at the first field that differs.
func SortForDisplay(links []Link) {
	sort.Slice(links, func(i, j int) bool {
		if links[i].Class != links[j].Class {
			return links[i].Class < links[j].Class
		}
		if links[i].Category != links[j].Category {
			return links[i].Category < links[j].Category
		}
		if links[i].Domain != links[j].Domain {
			return links[i].Domain < links[j].Domain
		}
		return links[i].HREF < links[j].HREF
	})
}
