// Package urlgroup detects repeated URL shapes (e.g. /admin/complex/9223/contacts
// and /admin/complex/9224/contacts) and folds them into one pattern
// (/admin/complex/{id}/contacts). Concrete URLs folded into a pattern become
// instances, and their differing path segments are surfaced as variable value
// sets (with numeric ranges where possible). Every variable slot carries the
// type it was grouped by: int, uuid, hash, slug or str (plain string literal).
package urlgroup

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"apimap/internal/linker"
)

var (
	digitsRe = regexp.MustCompile(`^[0-9]+$`)
	uuidRe   = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hashRe   = regexp.MustCompile(`^[0-9A-Za-z_-]{16,}$`)
	b64Re    = regexp.MustCompile(`^[0-9A-Za-z+/_-]+={0,2}$`)

	// Segment kinds reported by segmentKind.
	constKInt   = "int"
	constUUUID  = "uuid"
	constString = "string"
	constHash   = "hash"
	constBase64 = "base64"

	// maxBucketLen bounds the quadratic pair scan inside BuildURLs so a single
	// huge path bucket cannot stall the scan.
	maxBucketLen = 2000

	// groupStrings controls whether generic string-literal variables are allowed
	// to form patterns. When disabled only typed segments (int, uuid, hash,
	// base64) group; plain word values never fold.
	groupStrings = true
)

// SetGroupStrings enables or disables grouping by string literals. Disabling
// it drops patterns whose variable slots are plain words, keeping only the
// type-identifiable ids (numbers, uuids, hashes, base64 tokens). It affects
// BuildURLs/BuildLinks and thereby the crawl-time Detector too.
func SetGroupStrings(on bool) { groupStrings = on }

// segmentKind classifies a path segment as a resource identifier. Empty and
// template-shaped segments report "" (not groupable). Plain words fall through
// to "string", which is what enables grouping by string literals.
func segmentKind(seg string) string {
	if seg == "" || strings.ContainsAny(seg, "{}") {
		return ""
	}
	if digitsRe.MatchString(seg) {
		return constKInt
	}
	if uuidRe.MatchString(seg) {
		return constUUUID
	}
	if isBase64(seg) {
		return constBase64
	}
	if hashRe.MatchString(seg) {
		return constHash
	}
	return constString
}

// isBase64 reports whether a segment looks like a base64 payload: only the
// base64 alphabet, at least 8 chars, and a shaping signal (trailing '=' pad or
// an embedded '+'/'/'), which plain long words lack.
func isBase64(seg string) bool {
	if len(seg) < 8 || !b64Re.MatchString(seg) {
		return false
	}
	return strings.HasSuffix(seg, "=") || strings.ContainsAny(seg, "+/")
}

// Canonical normalizes a raw URL for grouping: scheme://host + path, query and
// fragment dropped, trailing slashes trimmed. The bool reports whether the raw
// value was usable at all.
func Canonical(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), true
}

// splitPath splits a URL path into its segments.
func splitPath(raw string) (host string, segs []string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Host == "" {
		return "", nil, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", nil, false
	}
	p := strings.Trim(u.Path, "/")
	if p == "" {
		return "", nil, false
	}
	return u.Host, strings.Split(p, "/"), true
}

// Var captures the observed values of one variable slot inside a pattern.
// Kind is the type of grouping: int, uuid, hash, slug or str.
type Var struct {
	Kind   string
	Values []string
	Nums   []int64
}

func (v Var) isNumeric() bool { return v.Nums != nil }

func (v Var) distinct() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(v.Values))
	for _, val := range v.Values {
		if !seen[val] {
			seen[val] = true
			out = append(out, val)
		}
	}
	return out
}

// Range renders the variable's observed values, preferring a compact numeric
// span ("9223..9224") when the values are consecutive and falling back to a
// truncated distinct list otherwise.
func (v Var) Range() string {
	if v.isNumeric() && len(v.Nums) > 0 {
		nums := distinctNums(v.Nums)
		sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
		if len(nums) >= 2 && nums[len(nums)-1]-nums[0]+1 == int64(len(nums)) {
			return fmt.Sprintf("%d..%d", nums[0], nums[len(nums)-1])
		}
		strs := make([]string, 0, len(nums))
		for _, n := range nums {
			strs = append(strs, strconv.FormatInt(n, 10))
		}
		return joinCut(strs, 8)
	}
	return joinCut(v.distinct(), 5)
}

func distinctNums(nums []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(nums))
	for _, n := range nums {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func joinCut(vals []string, _ int) string {
	return strings.Join(vals, ", ")
}

// VarLabel returns the display name for the i-th variable slot ("id", "id2", …).
func VarLabel(i int) string {
	if i == 0 {
		return "id"
	}
	return fmt.Sprintf("id%d", i+1)
}

// VarName returns the slot name used inside a pattern: a lone variable is
// "id", several variables are numbered "id1", "id2", …
func VarName(i, total int) string {
	if total == 1 {
		return "id"
	}
	return fmt.Sprintf("id%d", i+1)
}

// Group is one detected URL pattern with its concrete instances.
type Group struct {
	Domain  string
	Pattern string // e.g. https://host/admin/complex/{id}/contacts
	Count   int
	Vars    []Var

	segs []string // pattern segments, "{id}" markers at variable positions
	urls []string // canonical member URLs
}

// Segments returns the pattern path segments (public for tests).
func (g Group) Segments() []string { return g.segs }

// Members returns the canonical member URLs folded into the pattern.
func (g Group) Members() []string {
	out := make([]string, len(g.urls))
	copy(out, g.urls)
	return out
}

// GroupedBy returns the comma-joined set of variable kinds this pattern was
// grouped by (e.g. "int" or "int,str").
func (g Group) GroupedBy() string {
	seen := map[string]bool{}
	var out []string
	for _, v := range g.Vars {
		kind := v.Kind
		if kind == "" {
			kind = constString
		}
		if !seen[kind] {
			seen[kind] = true
			out = append(out, kind)
		}
	}
	if len(out) == 0 {
		return constString
	}
	return strings.Join(out, ",")
}

// VarDescriptor renders one variable slot as "id(int): 9223..9224".
func (g Group) VarDescriptor(i int) string {
	if i < 0 || i >= len(g.Vars) {
		return ""
	}
	v := g.Vars[i]
	kind := v.Kind
	if kind == "" {
		kind = constString
	}
	r := v.Range()
	if r == "" {
		return fmt.Sprintf("%s(%s)", VarName(i, len(g.Vars)), kind)
	}
	return fmt.Sprintf("%s(%s): %s", VarName(i, len(g.Vars)), kind, r)
}

// Annotation renders the grouping type header, e.g. "(int: id)" or
// "(int: id, string: id2)". Every variable slot is annotated with the type it
// was grouped by.
func (g Group) Annotation() string {
	parts := make([]string, 0, len(g.Vars))
	for i, v := range g.Vars {
		kind := v.Kind
		if kind == "" {
			kind = constString
		}
		parts = append(parts, kind+": "+VarName(i, len(g.Vars)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// ClusterRanges merges variable values across the patterns of one annotation
// cluster (patterns sharing the same kind/name set) and renders one range
// string per slot, e.g. "id: 9223..9224".
func ClusterRanges(groups []Group) []string {
	maxVars := 0
	for _, g := range groups {
		if len(g.Vars) > maxVars {
			maxVars = len(g.Vars)
		}
	}
	if maxVars == 0 {
		return nil
	}
	merged := make([][]string, maxVars)
	for _, g := range groups {
		for i, v := range g.Vars {
			merged[i] = append(merged[i], v.Values...)
		}
	}
	out := make([]string, 0, maxVars)
	for i, vals := range merged {
		v := Var{Kind: inferVarKind(vals), Values: vals}
		if v.Kind == constKInt {
			if nums, ok := parseNums(vals); ok {
				v.Nums = nums
			}
		}
		r := v.Range()
		if r == "" {
			r = joinCut(distinctVals(vals), 5)
		}
		out = append(out, VarName(i, maxVars)+": "+r)
	}
	return out
}

func distinctVals(vals []string) []string {
	return (Var{Values: vals}).distinct()
}

// Matches reports whether a concrete canonical URL belongs to this pattern:
// same host, same length, every literal segment equal. Variable slots accept
// any value (grouping covers numeric ids, uuids, hashes, slugs and string
// literals alike).
func (g Group) Matches(canonical string) bool {
	host, segs, ok := splitPath(canonical)
	if !ok || host != g.Domain || len(segs) != len(g.segs) {
		return false
	}
	for i, pat := range g.segs {
		if strings.Contains(pat, "{") {
			continue
		}
		if pat != segs[i] {
			return false
		}
	}
	return true
}

// BuildURLs folds raw URLs into patterns. minCount is the minimum number of
// concrete URLs required before a shape becomes a pattern.
func BuildURLs(raw []string, minCount int) []Group {
	if minCount < 2 {
		minCount = 2
	}
	segByURL := map[string][]string{}
	// host -> bucket key -> canonical urls. The bucket key is the first literal
	// segment plus the path depth, so /complex/*, /developer/* and /static/* are
	// paired within their own family instead of competing inside one flat list.
	byHost := map[string]map[string][]string{}
	for _, r := range raw {
		canonical, ok := Canonical(r)
		if !ok {
			continue
		}
		host, segs, ok := splitPath(canonical)
		if !ok {
			continue
		}
		if _, seen := segByURL[canonical]; seen {
			continue
		}
		segByURL[canonical] = segs
		if byHost[host] == nil {
			byHost[host] = map[string][]string{}
		}
		key := bucketKey(segs)
		byHost[host][key] = append(byHost[host][key], canonical)
	}

	accs := map[string]map[string]bool{}
	for host, buckets := range byHost {
		for _, urls := range buckets {
			if len(urls) < 2 {
				continue
			}
			if len(urls) > maxBucketLen {
				urls = urls[:maxBucketLen]
			}
			for i := 0; i < len(urls); i++ {
				a := segByURL[urls[i]]
				for j := i + 1; j < len(urls); j++ {
					b := segByURL[urls[j]]
					if !compatiblePair(a, b) {
						continue
					}
					key := maskKey(host, maskedSegs(a, b))
					if accs[key] == nil {
						accs[key] = map[string]bool{}
					}
					accs[key][urls[i]] = true
					accs[key][urls[j]] = true
				}
			}
		}
	}

	var groups []Group
	for key, members := range accs {
		if len(members) < minCount {
			continue
		}
		g := buildGroup(key, members, segByURL)
		if catchAll(g) {
			continue
		}
		if !groupStrings && hasStringVar(g) {
			continue
		}
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Domain != groups[j].Domain {
			return groups[i].Domain < groups[j].Domain
		}
		return groups[i].Pattern < groups[j].Pattern
	})
	return groups
}

const maskSep = "\x1f"

// bucketKey groups sibling paths that can actually form a pattern together:
// the path depth plus the first literal segment. Keeping /complex/* apart
// from /static/* stops one huge bucket from crowding the others out and makes
// the resulting patterns meaningful ("/complex/{id}", not "/{a}/{b}").
// A first segment that is itself a typed identifier (int/uuid/hash/base64)
// belongs to the variable space, so those bucket by depth alone.
func bucketKey(segs []string) string {
	first := segs[0]
	switch segmentKind(first) {
	case constKInt, constUUUID, constHash, constBase64:
		first = ""
	}
	return strconv.Itoa(len(segs)) + maskSep + first
}

// maskKey builds a comparable string key from a host and a masked segment
// shape ("" marks a variable position).
func maskKey(host string, segs []string) string {
	return host + maskSep + strings.Join(segs, maskSep)
}

// parseMaskKey reverses maskKey into the host and masked segments.
func parseMaskKey(key string) (string, []string) {
	parts := strings.Split(key, maskSep)
	return parts[0], parts[1:]
}

// maskedSegs returns the merged segment shape of a compatible pair: literals
// where both agree, "" where they differ (the variable slot).
func maskedSegs(a, b []string) []string {
	segs := make([]string, len(a))
	for i := range a {
		if a[i] == b[i] {
			segs[i] = a[i]
		} else {
			segs[i] = ""
		}
	}
	return segs
}

// compatiblePair reports whether two segment lists can share a pattern. Any
// differing segment is allowed; the variable kind is derived from the observed
// values afterwards.
func compatiblePair(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}

// inferVarKind derives the grouping type from the observed values. Mixed shapes
// collapse to "str".
func inferVarKind(values []string) string {
	kind := ""
	for _, v := range values {
		k := segmentKind(v)
		if k == "" {
			return constString
		}
		if kind == "" {
			kind = k
		} else if k != kind {
			return constString
		}
	}
	if kind == "" {
		return constString
	}
	return kind
}

// hasStringVar reports whether any variable slot of the group is a plain
// string literal (used when string grouping is disabled).
func hasStringVar(g Group) bool {
	for _, v := range g.Vars {
		if v.Kind == constString {
			return true
		}
	}
	return false
}

// catchAll reports whether the pattern has no literal segment at all
// (e.g. {id}/{id2}/{id3} under a bare host). Such catch-all shapes absorb
// unrelated endpoints and convey nothing, so they are never emitted.
func catchAll(g Group) bool {
	for _, s := range g.segs {
		if !strings.Contains(s, "{") {
			return false
		}
	}
	return true
}

func buildGroup(key string, members map[string]bool, segByURL map[string][]string) Group {
	host, keySegs := parseMaskKey(key)
	urls := make([]string, 0, len(members))
	for u := range members {
		urls = append(urls, u)
	}
	sort.Strings(urls)

	pat := make([]string, len(keySegs))
	slots := 0
	for _, lit := range keySegs {
		if lit == "" {
			slots++
		}
	}
	vars := make([]Var, slots)
	slot := 0
	for i, lit := range keySegs {
		if lit != "" {
			pat[i] = lit
			continue
		}
		vars[slot] = Var{}
		slot++
	}
	for _, u := range urls {
		ss := segByURL[u]
		vi := 0
		for k, lit := range keySegs {
			if lit != "" {
				continue
			}
			vars[vi].Values = append(vars[vi].Values, ss[k])
			vi++
		}
	}
	for i := range vars {
		vars[i].Kind = inferVarKind(vars[i].Values)
		if vars[i].Kind == constKInt {
			if nums, ok := parseNums(vars[i].Values); ok {
				vars[i].Nums = nums
			}
		}
	}
	// Typed placeholders: /developer/{id: int}, /webp/{id1: int}/{id2: int}.
	slot = 0
	for i, lit := range keySegs {
		if lit == "" {
			pat[i] = "{" + VarName(slot, slots) + ": " + vars[slot].Kind + "}"
			slot++
		}
	}

	base := "https://" + host
	if u, err := url.Parse(urls[0]); err == nil && u.Scheme != "" {
		base = u.Scheme + "://" + host
	}

	return Group{
		Domain:  host,
		Pattern: base + "/" + strings.Join(pat, "/"),
		Count:   len(members),
		Vars:    vars,
		segs:    pat,
		urls:    urls,
	}
}

func parseNums(values []string) ([]int64, bool) {
	nums := make([]int64, 0, len(values))
	for _, v := range values {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, false
		}
		nums = append(nums, n)
	}
	return nums, true
}

// LinkKey returns the URL used for grouping identity of a link.
func LinkKey(l linker.Link) string {
	if l.Resolved != "" {
		return l.Resolved
	}
	return l.HREF
}

// BuildLinks folds a set of links into patterns (see BuildURLs). API links are
// deliberately left alone: endpoints are reported as API contracts with their
// own methods, parameters and response schemas instead of being collapsed into
// a URL pattern.
func BuildLinks(links []linker.Link, minCount int) []Group {
	raw := make([]string, 0, len(links))
	for _, l := range links {
		if l.Category == linker.CategoryAPI || l.Category == linker.CategoryDynamic {
			continue
		}
		raw = append(raw, LinkKey(l))
	}
	return BuildURLs(raw, minCount)
}

// WithoutMembers drops every link that was folded into one of the groups.
func WithoutMembers(links []linker.Link, groups []Group) []linker.Link {
	out := make([]linker.Link, 0, len(links))
	for _, l := range links {
		c, ok := Canonical(LinkKey(l))
		if !ok || !memberOf(c, groups) {
			out = append(out, l)
		}
	}
	return out
}

func memberOf(canonical string, groups []Group) bool {
	for _, g := range groups {
		for _, u := range g.urls {
			if u == canonical {
				return true
			}
		}
	}
	return false
}

// VarCount returns the number of variable slots in the pattern.
func (g Group) VarCount() int { return len(g.Vars) }

// MatchURL returns the canonical pattern that best absorbs the given raw URL,
// or "" if no group matches. The best match is the narrowest general bucket:
// fewest variable slots first, then the longest run of trailing literal
// segments (so /complex/9223/contacts folds into /complex/{id}/contacts, not
// into /complex/9223/{id} or a wider two-var shape).
func MatchURL(raw string, groups []Group) string {
	c, ok := Canonical(raw)
	if !ok {
		return ""
	}
	csegs := splitSegs(c)
	var best string
	var bestVars int
	var bestTrail int
	var bestCount int
	for _, g := range groups {
		if !g.Matches(c) {
			continue
		}
		v := g.VarCount()
		t := trailingLiterals(g, csegs)
		if best == "" || v < bestVars || (v == bestVars && t > bestTrail) ||
			(v == bestVars && t == bestTrail && g.Count > bestCount) {
			best = g.Pattern
			bestVars = v
			bestTrail = t
			bestCount = g.Count
		}
	}
	return best
}

func splitSegs(canonical string) []string {
	_, segs, _ := splitPath(canonical)
	return segs
}

// trailingLiterals counts how many consecutive literal pattern segments match
// the URL's trailing segments.
func trailingLiterals(g Group, urlSegs []string) int {
	if len(g.segs) != len(urlSegs) {
		return 0
	}
	n := 0
	for i := len(g.segs) - 1; i >= 0; i-- {
		if strings.Contains(g.segs[i], "{") || g.segs[i] != urlSegs[i] {
			return n
		}
		n++
	}
	return n
}

// Detector incrementally confirms patterns across crawl batches and reports
// which concrete URLs belong to a confirmed pattern so callers can drop them
// from the crawl queue. A rolling window of recent unmatched URLs lets
// cross-batch patterns form without unbounded memory.
type Detector struct {
	mu        sync.Mutex
	minCount  int
	groups    []Group
	samples   []string
	sampleCap int
}

// NewDetector creates a ready Detector for the given confirmation threshold.
func NewDetector(minCount int) *Detector {
	if minCount < 2 {
		minCount = 2
	}
	return &Detector{minCount: minCount, sampleCap: 250}
}

// Feed folds a batch of raw candidate URLs into the detector and returns the
// canonical URLs that now belong to a confirmed pattern.
func (d *Detector) Feed(keys []string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, k := range keys {
		if c, ok := Canonical(k); ok {
			d.samples = append(d.samples, c)
		}
	}
	if len(d.samples) > d.sampleCap {
		d.samples = d.samples[len(d.samples)-d.sampleCap:]
	}

	for _, g := range BuildURLs(d.samples, d.minCount) {
		d.mergeGroup(g)
	}

	blocked := map[string]bool{}
	for _, k := range keys {
		if c, ok := Canonical(k); ok && d.contains(c) {
			blocked[c] = true
		}
	}
	out := make([]string, 0, len(blocked))
	for c := range blocked {
		out = append(out, c)
	}
	return out
}

func (d *Detector) contains(canonical string) bool {
	for _, g := range d.groups {
		if g.Matches(canonical) {
			return true
		}
	}
	return false
}

func (d *Detector) mergeGroup(g Group) {
	for _, existing := range d.groups {
		if existing.Domain == g.Domain && existing.Pattern == g.Pattern {
			return
		}
	}
	d.groups = append(d.groups, g)
}

// Groups returns the currently confirmed patterns.
func (d *Detector) Groups() []Group {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Group(nil), d.groups...)
}
