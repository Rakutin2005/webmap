package linker

import (
	"regexp"
	"strings"
)

type Category int

const (
	CategoryUnknown Category = iota
	CategoryWebAsset
	CategoryWebPage
	CategoryAPI
	CategoryDynamic
)

type LinkType int

const (
	LinkTypeUnknown LinkType = iota
	LinkTypeAbsolute
	LinkTypeRelative
	LinkTypeWeb
)

type URLClass int

const (
	ClassNormal URLClass = iota
	ClassWAF
	ClassCDN
	ClassCache
	ClassNoise
)

type APIDetail struct {
	HTTPMethod  string
	Arguments   string
	MatchSource string
}

type ParamVariant struct {
	Query string
}

type Link struct {
	HREF     string
	Resolved string
	Domain   string
	Category Category
	LinkType LinkType
	Class    URLClass
	// Depth is the crawl level at which this link was found: the page itself
	// is 0, a link on it is 1, and so on. Zero also means "not from a crawl",
	// which is the case for links recovered from a bundle or a sandbox run.
	Depth         int
	HasParams     bool
	ParamVariants []ParamVariant
	SourceURL     string
	Tag           string
	APIDetails    []APIDetail
	// Synthesized marks a node a snapshot added so that a relation had
	// something to point at: the page was fetched, but nothing ever linked to
	// it, so no report that lists links has ever printed it as one. A reader
	// that lists every node of a file would otherwise show a page the scan
	// itself never listed, and the two reports would not be the same report.
	// A crawl never sets it; only a stored set does.
	Synthesized bool
}

var (
	CDNDomains    = defaultCDN()
	CachePatterns = defaultCache()
	WAFPatterns   = defaultWAF()
)

func defaultCDN() []string {
	return []string{
		"cdnjs.cloudflare.com",
		"unpkg.com",
		"jsdelivr.net",
		"cdn.jsdelivr.net",
		"cdnjs.com",
		"stackpathcdn.com",
		"bootstrapcdn.com",
		"cdn.rawgit.com",
		"cdn.statically.io",
		"cdn.ampproject.org",
		"cdn.jsdelivr.",
	}
}

func defaultCache() []string {
	return []string{
		"?v=", "&v=", "?ver=", "&ver=", "?version=", "&version=",
		"cache-buster", "cachebuster",
	}
}

func defaultWAF() []string {
	return []string{
		"/cdn-cgi/",
		"cloudflare",
		"__cdn",
		"waf",
		"bot-detect",
		"recaptcha",
		"challenge",
	}
}

var (
	apiPatterns     = defaultAPIPatterns()
	assetPatterns   = defaultAssetPatterns()
	assetExtensions = defaultAssetExtensions()
)

func defaultAPIPatterns() []string {
	return []string{
		"/api/", "/v1/", "/v2/", "/v3/",
		"/graphql", "/rest/", "/rpc/",
		"/ajax", "/xhr/", "/fetch/",
		"_api",
		"api.php", "ajax.php", "endpoint.php",
		"rpc.php", "rest.php", "service.php",
		"handler.php", "gateway.php", "soap.php",
		"action.php", "route.php", "wp-json",
		"/bitrix/services/", "/services/main/ajax",
		"/wp-admin/admin-ajax",
		"/rest-api", "/restapi",
		"/endpoint", "/webhook",
		"/socket.io", "/sse/",
	}
}

func defaultAssetPatterns() []string {
	return []string{
		"google-fonts", "fonts.googleapis", "fonts.gstatic",
		"cdnjs.cloudflare", "unpkg.com", "jsdelivr.net",
		"static.", "assets.", "/assets/", "/static/",
		".min.", ".bundle.",
	}
}

func defaultAssetExtensions() []string {
	return []string{
		".css", ".scss", ".sass", ".less",
		".js", ".mjs", ".ts",
		".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp", ".avif",
		".woff", ".woff2", ".ttf", ".otf", ".eot",
		".mp4", ".webm", ".ogg", ".mp3", ".wav", ".flac",
		".pdf", ".doc", ".docx", ".zip", ".rar",
		".json", ".xml",
	}
}

func SetPatterns(api, asset, waf, cdn, cache []string, assetExts []string) {
	if len(api) > 0 {
		apiPatterns = api
	}
	if len(asset) > 0 {
		assetPatterns = asset
	}
	if len(waf) > 0 {
		WAFPatterns = waf
	}
	if len(cdn) > 0 {
		CDNDomains = cdn
	}
	if len(cache) > 0 {
		CachePatterns = cache
	}
	if len(assetExts) > 0 {
		assetExtensions = assetExts
	}
}

func ResetPatterns() {
	apiPatterns = defaultAPIPatterns()
	assetPatterns = defaultAssetPatterns()
	assetExtensions = defaultAssetExtensions()
	WAFPatterns = defaultWAF()
	CDNDomains = defaultCDN()
	CachePatterns = defaultCache()
}

func IsWAFURL(u string) bool {
	lower := strings.ToLower(u)
	for _, p := range WAFPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func IsCDNURL(u string) bool {
	lower := strings.ToLower(u)
	for _, d := range CDNDomains {
		if strings.Contains(lower, d) {
			return true
		}
	}
	return false
}

func IsCacheURL(u string) bool {
	lower := strings.ToLower(u)
	for _, p := range CachePatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func IsNoiseEntry(s string) bool {
	if strings.Contains(s, ";") {
		return true
	}
	if strings.Contains(s, "${") {
		return true
	}
	trimmed := strings.TrimSpace(s)
	if strings.Contains(trimmed, " ") {
		return true
	}
	if strings.Contains(s, "=>") {
		return true
	}
	return false
}

func ClassifyURL(href string) URLClass {
	if IsWAFURL(href) {
		return ClassWAF
	}
	if IsCDNURL(href) {
		return ClassCDN
	}
	if IsCacheURL(href) {
		return ClassCache
	}
	if !looksLikeURL(href) || IsNoiseEntry(href) {
		return ClassNoise
	}
	return ClassNormal
}

var linkTagRegex = regexp.MustCompile(`(?i)<a[^>]+href=["']([^"']+)["'][^>]*>`)
var scriptTagRegex = regexp.MustCompile(`(?i)<script[^>]+src=["']([^"']+)["'][^>]*>`)
var linkTagRegex2 = regexp.MustCompile(`(?i)<link[^>]+href=["']([^"']+)["'][^>]*>`)
var imgTagRegex = regexp.MustCompile(`(?i)<img[^>]+src=["']([^"']+)["'][^>]*>`)
var sourceTagRegex = regexp.MustCompile(`(?i)<source[^>]+src=["']([^"']+)["'][^>]*>`)
var videoTagRegex = regexp.MustCompile(`(?i)<video[^>]+src=["']([^"']+)["'][^>]*>`)
var audioTagRegex = regexp.MustCompile(`(?i)<audio[^>]+src=["']([^"']+)["'][^>]*>`)
var iframeTagRegex = regexp.MustCompile(`(?i)<iframe[^>]+src=["']([^"']+)["'][^>]*>`)
var cssImportRegex = regexp.MustCompile(`(?i)@import\s+['"]([^"']+)['"]`)

// jsImportRegex is applied to raw JS bundels, so the leading keyword must not
// be a fragment of a longer identifier (le.from, created_from). RE2 has no
// negative lookbehind, so the excluded prefix class is consumed instead.
var jsImportRegex = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_.$])(?:import|export|from|require)\b\s*\(?\s*["']([^"']{1,512})["']`)

func Parse(body string, sourceURL string) []Link {
	return ParseWithRefs(body, sourceURL, nil)
}

// ParseWithRefs is Parse plus, when ix is non-nil, a reference for every link it
// finds: the offset of the match, its line and column, and the text around it,
// filed under sourceURL.
//
// The reference work is behind a nil check so a scan that did not ask for
// references runs the same regex pass it always did, with no line counting and
// no per-link snippet. That is the difference between a flag that costs
// something and a flag that costs nothing until it is used.
func ParseWithRefs(body string, sourceURL string, ix *RefIndex) []Link {
	links := make([]Link, 0)

	links = append(links, findLinks(linkTagRegex, body, sourceURL, "a", ix)...)
	links = append(links, findLinks(scriptTagRegex, body, sourceURL, "script", ix)...)
	links = append(links, findLinks(linkTagRegex2, body, sourceURL, "link", ix)...)
	links = append(links, findLinks(imgTagRegex, body, sourceURL, "img", ix)...)
	links = append(links, findLinks(sourceTagRegex, body, sourceURL, "source", ix)...)
	links = append(links, findLinks(videoTagRegex, body, sourceURL, "video", ix)...)
	links = append(links, findLinks(audioTagRegex, body, sourceURL, "audio", ix)...)
	links = append(links, findLinks(iframeTagRegex, body, sourceURL, "iframe", ix)...)
	links = append(links, findLinks(cssImportRegex, body, sourceURL, "css-import", ix)...)
	links = append(links, findLinks(jsImportRegex, body, sourceURL, "js-import", ix)...)

	return deduplicate(links)
}

func findLinks(regex *regexp.Regexp, body string, sourceURL string, tag string, ix *RefIndex) []Link {
	var links []Link
	if ix == nil {
		// The ordinary path: matches without positions, which is all the
		// scan needs when nothing is recording where things were found.
		matches := regex.FindAllStringSubmatch(body, -1)
		links = make([]Link, 0, len(matches))
		for _, match := range matches {
			if l, ok := linkFromMatch(match, sourceURL, tag); ok {
				links = append(links, l)
			}
		}
		return links
	}

	// With references, the submatch indices carry the byte offsets of each
	// group, so the same match that becomes a link also becomes a position.
	indices := regex.FindAllStringSubmatchIndex(body, -1)
	links = make([]Link, 0, len(indices))
	for _, idx := range indices {
		if len(idx) < 4 || idx[2] < 0 {
			continue
		}
		// idx[2], idx[3] bound group 1, the URL itself; the reference points
		// at the URL rather than the whole tag, because that is the token a
		// reader will search for.
		match := make([]string, 2)
		match[1] = body[idx[2]:idx[3]]
		if l, ok := linkFromMatch(match, sourceURL, tag); ok {
			links = append(links, l)
		}
		line, col := lineColumn(body, idx[2])
		ix.Add(sourceURL, "", Reference{
			Target:  match[1],
			Offset:  idx[2],
			Line:    line,
			Column:  col,
			Snippet: makeSnippet(body, idx[2], idx[3]-idx[2]),
		})
	}
	return links
}

// linkFromMatch builds a link from a regex match whose first submatch is the
// href, applying the same accept/reject rules for every tag. A rejected match
// yields no link and therefore no reference: the position of something the scan
// chose to ignore is not evidence of anything.
func linkFromMatch(match []string, sourceURL string, tag string) (Link, bool) {
	if len(match) < 2 {
		return Link{}, false
	}
	href := match[1]
	href = strings.TrimSpace(href)
	if tag == "js-import" && !isValidModuleSpecifier(href) {
		return Link{}, false
	}
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "tel:") {
		return Link{}, false
	}
	if !hasValidURLChars(href) {
		return Link{}, false
	}

	linkType := determineLinkType(href)
	category := categorize(href)
	class := ClassifyURL(href)
	hasParams := false
	if idx := strings.IndexByte(href, '?'); idx >= 0 && idx < len(href)-1 && !strings.HasPrefix(href, "?") {
		hasParams = true
	}

	return Link{
		HREF:      href,
		Category:  category,
		LinkType:  linkType,
		Class:     class,
		HasParams: hasParams,
		SourceURL: sourceURL,
		Tag:       tag,
	}, true
}

func determineLinkType(href string) LinkType {
	if strings.HasPrefix(href, "//") {
		return LinkTypeWeb
	}
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return LinkTypeAbsolute
	}
	return LinkTypeRelative
}

// MatchesAPIPattern reports whether the URL path contains any configured API
// pattern (e.g. "/api/", "ajax.php", "/rest/", "/bitrix/services/").
func MatchesAPIPattern(href string) bool {
	lower := strings.ToLower(href)
	for _, p := range apiPatterns {
		if p == "" {
			continue
		}
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func categorize(href string) Category {
	lowerHREF := strings.ToLower(href)

	pathOnly := lowerHREF
	hasQuery := false
	if idx := strings.IndexByte(pathOnly, '?'); idx >= 0 {
		pathOnly = pathOnly[:idx]
		hasQuery = true
	}

	// Static assets identified purely by file extension (css, images, fonts,
	// media, .js, .json, ...) win first so that e.g. api.js stays an asset.
	for _, ext := range assetExtensions {
		if strings.HasSuffix(pathOnly, ext) {
			return CategoryWebAsset
		}
	}

	// API endpoints: explicit API path patterns, or dynamic .php handlers.
	if MatchesAPIPattern(lowerHREF) {
		return CategoryAPI
	}
	if isPHPApiEndpoint(href, hasQuery) {
		return CategoryAPI
	}

	for _, pattern := range assetPatterns {
		if strings.Contains(lowerHREF, pattern) {
			return CategoryWebAsset
		}
	}

	if strings.HasSuffix(pathOnly, ".html") || strings.HasSuffix(pathOnly, ".htm") || strings.HasSuffix(pathOnly, "/") {
		return CategoryWebPage
	}

	return CategoryWebPage
}

func isPHPApiEndpoint(href string, hasParams bool) bool {
	lower := strings.ToLower(href)
	if !strings.HasSuffix(lower, ".php") {
		return false
	}
	if hasParams || strings.Contains(href, "?") {
		return true
	}
	phpAPIPatterns := []string{
		"/api", "/ajax", "/rest", "/rpc", "/xhr",
		"/handler", "/gateway", "/service", "/endpoint",
		"/action", "/process", "/submit", "/callback",
		"/webhook", "/notify", "/push", "/pull",
		"/login", "/auth", "/token", "/oauth",
		"/search", "/query", "/lookup",
	}
	for _, p := range phpAPIPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func deduplicate(links []Link) []Link {
	seen := make(map[string]bool)
	result := make([]Link, 0)

	for _, link := range links {
		key := link.HREF + link.SourceURL
		if !seen[key] {
			seen[key] = true
			result = append(result, link)
		}
	}

	return result
}

func (c Category) String() string {
	switch c {
	case CategoryWebAsset:
		return "web-asset"
	case CategoryWebPage:
		return "web-page"
	case CategoryAPI:
		return "API"
	case CategoryDynamic:
		return "DYNAMIC"
	default:
		return "unknown"
	}
}

func (t LinkType) String() string {
	switch t {
	case LinkTypeAbsolute:
		return "absolute"
	case LinkTypeRelative:
		return "relative"
	case LinkTypeWeb:
		return "web"
	default:
		return "unknown"
	}
}

func (c URLClass) String() string {
	switch c {
	case ClassWAF:
		return "WAF"
	case ClassCDN:
		return "CDN"
	case ClassCache:
		return "cache"
	case ClassNoise:
		return "noise"
	default:
		return "normal"
	}
}

func looksLikeURL(s string) bool {
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "//") {
		return true
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return true
	}
	if strings.Contains(s, "/") {
		return true
	}
	if strings.Contains(s, ".") && !looksLikeBareDomain(s) {
		return true
	}
	if strings.Contains(s, ".") && !strings.Contains(s, " ") && strings.Count(s, ".") == 1 && len(s) > 3 && len(s) < 50 && !strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return true
	}
	return false
}

func looksLikeBareDomain(s string) bool {
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "http") || strings.HasPrefix(s, "//") {
		return false
	}
	hasLetter := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				hasLetter = true
			}
			continue
		}
		return false // non-domain character found
	}
	return hasLetter
}

func hasValidURLChars(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return true
		}
	}
	return false
}

// isValidModuleSpecifier reports whether a jsImportRegex capture is a
// plausible module specifier instead of a fragment of minified code (e.g. a
// switch label, an identifier near a quote, or an interpolation). The regex
// alone cannot decide this because it runs on raw bundle text.
func isValidModuleSpecifier(s string) bool {
	if s == "" || len(s) > 512 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			continue
		case c == '/', c == '.', c == '_', c == '-', c == '@', c == '~',
			c == '?', c == '#', c == '=', c == '%', c == '+', c == ':':
			continue
		default:
			return false
		}
	}
	if looksLikeURL(s) || strings.Contains(s, "/") || strings.Contains(s, "@") {
		return true
	}
	// Bare npm-style module names (vue, lodash) are lowercase; camelCase
	// words such as getAllResponseHeaders are code identifiers, not imports.
	// URL metacharacters without a path context (e.g. ":case") are fragments.
	if strings.ContainsAny(s, ":?#=") {
		return false
	}
	return strings.ToLower(s) == s
}

var (
	tmplLiteralRegex = regexp.MustCompile("`([^`\\\\]*(?:\\\\.[^`\\\\]*)*)`")
	tmplInterpRegex  = regexp.MustCompile(`\$\{([^}]*)\}`)
)

// ParamKind separates a query parameter from a multipart form field: they are
// built by the same ".set('/.append(' shape but mean very different things.
type ParamKind int

const (
	ParamQuery ParamKind = iota
	ParamForm
	ParamUnknown
)

func (k ParamKind) String() string {
	switch k {
	case ParamQuery:
		return "query"
	case ParamForm:
		return "form"
	case ParamUnknown:
		return "unknown"
	}
	return "unknown"
}

// ParamOwner says what the builder was writing into, which decides whether a
// name belongs to an API request or to the page's own query string.
type ParamOwner int

const (
	OwnerUnknown ParamOwner = iota
	OwnerLocation
	OwnerRequest
)

func (o ParamOwner) String() string {
	switch o {
	case OwnerLocation:
		return "location"
	case OwnerRequest:
		return "request"
	case OwnerUnknown:
		return "unknown"
	}
	return "unknown"
}

// ParamRef is one parameter name recovered from a request-building call, with
// enough context to tell where it belongs.
type ParamRef struct {
	// Endpoints lists the request endpoints a recovered name was bound to.
	// Without it a name is a bare string: it says what exists, but not which
	// URL it belongs to.
	Endpoints []string
	// Carrier names the URL expression a name travels through when the code
	// never spells the endpoint out ("this.ajaxUrl"). It is what can be said
	// truthfully: the request exists, its literal address does not.
	Carrier string
	Name    string
	Kind    ParamKind
	Owner   ParamOwner
	Count   int
}

func (p ParamRef) String() string { return p.Name }

// KindLabel names the parameter kind for the report.
func (p ParamRef) KindLabel() string {
	switch p.Kind {
	case ParamQuery:
		return "query"
	case ParamForm:
		return "form"
	}
	return "unknown"
}

// OwnerLabel says what the builder feeds, which is the fact that decides
// whether a name belongs to an API request or to the page's own query string.
func (p ParamRef) OwnerLabel() string {
	switch p.Owner {
	case OwnerLocation:
		return "page query state"
	case OwnerRequest:
		return "request"
	}
	return "unassigned"
}

// AnalyzeJS digs endpoint formation signals out of raw JS bundle text. It
// returns two kinds of findings that structured parsing routinely misses on
// minified code:
//
//   - url-tmpl links: backtick templates with interpolation that look like a
//     URL/endpoint (kept with {var} placeholders so the dynamic shape is not
//     lost);
//   - parameter names discovered through the URLSearchParams-style
//     .set()/.append() builders that assemble runtime query strings, each
//     classified by kind and by the builder it is attached to.
//
// Nothing here performs network I/O: it is purely lexical analysis.
func AnalyzeJS(js string, sourceURL string) (templates []Link) {
	return AnalyzeJSWithRefs(js, sourceURL, nil)
}

// AnalyzeJSWithRefs is AnalyzeJS plus, when ix is non-nil, a reference for each
// template literal, pointing at the offset of the literal in the script. A
// dynamic URL found in a bundle is one of the harder things to trace back by
// hand, so the position is worth as much here as it is for an HTML attribute.
func AnalyzeJSWithRefs(js string, sourceURL string, ix *RefIndex) (templates []Link) {

	seenTmpl := make(map[string]bool)
	for _, m := range tmplLiteralRegex.FindAllStringSubmatchIndex(js, -1) {
		if len(m) < 4 || m[2] < 0 || !strings.Contains(js[m[2]:m[3]], "${") {
			continue
		}
		raw := js[m[2]:m[3]]
		norm := tmplInterpRegex.ReplaceAllStringFunc(raw, templatePlaceholder)
		norm = cleanDynamicTemplate(norm)
		if norm == "" || strings.Count(norm, "{") > 8 {
			continue
		}
		if seenTmpl[norm] {
			continue
		}
		seenTmpl[norm] = true
		category := CategoryDynamic
		if MatchesAPIPattern(norm) {
			category = CategoryAPI
		}
		linkType := LinkTypeRelative
		if strings.HasPrefix(norm, "http://") || strings.HasPrefix(norm, "https://") {
			linkType = LinkTypeAbsolute
		}
		templates = append(templates, Link{
			HREF:      norm,
			Category:  category,
			LinkType:  linkType,
			SourceURL: sourceURL,
			Tag:       "url-tmpl",
		})
		if ix != nil {
			line, col := lineColumn(js, m[2])
			ix.Add(sourceURL, "", Reference{
				Target:  norm,
				Offset:  m[2],
				Line:    line,
				Column:  col,
				Snippet: makeSnippet(js, m[2], m[3]-m[2]),
			})
		}
	}
	return templates
}

// paramReceiverRe captures the object a builder call is invoked on.
var paramReceiverRe = regexp.MustCompile(`([A-Za-z_$][\w$]{0,40})\s*\.\s*(?:set|append)\s*\(\s*["']([A-Za-z_$][\w$]{0,63})["']`)

// paramWindowRe matches the request/builder constructors that decide who owns
// a parameter. It is applied to the text surrounding a builder call.
var (
	paramRequestRe  = regexp.MustCompile(`\bfetch\s*\(|\bXMLHttpRequest\b|\.ajax\s*\(|\baxios\b|\$\s*\.\s*(?:get|post|getJSON)\s*\(`)
	paramBuilderRe  = regexp.MustCompile(`\bURLSearchParams\b|\bnew\s+URL\s*\(`)
	paramLocationRe = regexp.MustCompile(`\blocation\b|\bwindow\.location\b|\bdocument\.URL\b`)
)

// queryReceivers and formReceivers name the builders we can recognise. The
// sets are intentionally generous: a missed name costs more than a name that
// is filed under "unknown".
var (
	queryReceivers = map[string]bool{
		"urlsearchparams": true, "searchparams": true, "params": true,
		"query": true, "qs": true, "sp": true, "search": true,
		"searchparams2": true, "usp": true, "urlparams": true,
		"filter": true, "filters": true, "sort": true, "sorting": true,
		"paging": true, "pager": true, "queryparams": true, "queryparams2": true,
	}
	formReceivers = map[string]bool{
		"formdata": true, "form": true, "data": true, "fields": true,
		"multipart": true, "payload": true, "fd": true,
	}
)

// templatePlaceholder renders a single template interpolation as a readable
// path placeholder ({id}, {page}, {le.status}) so a dynamic endpoint shape
// can still be examined. Complex expressions collapse to {…}. The input is the
// full ${…} match (ReplaceAllStringFunc passes whole matches, not groups).
func templatePlaceholder(src string) string {
	name := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(src), "}"), "${")
	name = strings.TrimSpace(name)
	if name == "" {
		return "{…}"
	}
	if strings.ContainsAny(name, " ()+*/&|?:,<>[]{}=!\"'`;\\$") {
		return "{…}"
	}
	return "{" + name + "}"
}

// cleanDynamicTemplate keeps only templates that look like URL paths/endpoints
// and removes incidental whitespace and escapes from the normalized form.
func cleanDynamicTemplate(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t") {
		return ""
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "//") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "/") {
		return s
	}
	// ${apiBase}/api/users normalizes to {apiBase}/api/users: the dynamic
	// scheme/host is fine as long as a real path follows.
	if strings.HasPrefix(s, "{") {
		if end := strings.IndexByte(s, '}'); end > 0 && end < len(s)-1 {
			rest := s[end+1:]
			if strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, "./") ||
				strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") ||
				strings.HasPrefix(rest, "//") {
				return s
			}
		}
	}
	return ""
}
