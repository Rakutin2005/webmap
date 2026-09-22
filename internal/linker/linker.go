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
	HREF          string
	Resolved      string
	Domain        string
	Category      Category
	LinkType      LinkType
	Class         URLClass
	HasParams     bool
	ParamVariants []ParamVariant
	SourceURL     string
	Tag           string
	APIDetails    []APIDetail
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
var jsImportRegex = regexp.MustCompile(`(?i)(?:import|export|from|require)\s*\(?\s*['"]([^"']+)['"]`)

func Parse(body string, sourceURL string) []Link {
	links := make([]Link, 0)

	links = append(links, findLinks(linkTagRegex, body, sourceURL, "a")...)
	links = append(links, findLinks(scriptTagRegex, body, sourceURL, "script")...)
	links = append(links, findLinks(linkTagRegex2, body, sourceURL, "link")...)
	links = append(links, findLinks(imgTagRegex, body, sourceURL, "img")...)
	links = append(links, findLinks(sourceTagRegex, body, sourceURL, "source")...)
	links = append(links, findLinks(videoTagRegex, body, sourceURL, "video")...)
	links = append(links, findLinks(audioTagRegex, body, sourceURL, "audio")...)
	links = append(links, findLinks(iframeTagRegex, body, sourceURL, "iframe")...)
	links = append(links, findLinks(cssImportRegex, body, sourceURL, "css-import")...)
	links = append(links, findLinks(jsImportRegex, body, sourceURL, "js-import")...)

	return deduplicate(links)
}

func findLinks(regex *regexp.Regexp, body string, sourceURL string, tag string) []Link {
	matches := regex.FindAllStringSubmatch(body, -1)
	links := make([]Link, 0, len(matches))

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		href := match[1]
		href = strings.TrimSpace(href)
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "tel:") {
			continue
		}
		if !hasValidURLChars(href) {
			continue
		}

		linkType := determineLinkType(href)
		category := categorize(href)
		class := ClassifyURL(href)
		hasParams := false
		if idx := strings.IndexByte(href, '?'); idx >= 0 && idx < len(href)-1 && !strings.HasPrefix(href, "?") {
			hasParams = true
		}

		links = append(links, Link{
			HREF:      href,
			Category:  category,
			LinkType:  linkType,
			Class:     class,
			HasParams: hasParams,
			SourceURL: sourceURL,
			Tag:       tag,
		})
	}

	return links
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
