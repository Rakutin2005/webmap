package patterns

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type PatternSet struct {
	APIPatterns     []string            `json:"api_patterns"`
	AssetPatterns   []string            `json:"asset_patterns"`
	AssetExtensions []string            `json:"asset_extensions"`
	WAFPatterns     []string            `json:"waf_patterns"`
	CDNDomains      []string            `json:"cdn_domains"`
	CachePatterns   []string            `json:"cache_patterns"`
	Tags            map[string][]string `json:"tags"`
	TagPriority     []string            `json:"-"`
}

func Defaults() *PatternSet {
	return &PatternSet{
		APIPatterns: []string{
			"/api/", "/v1/", "/v2/", "/v3/",
			"/graphql", "/rest/", "/rpc/",
			"/ajax", "/xhr/", "/fetch/",
			"_api", "api.js",
			"api.php", "ajax.php", "endpoint.php",
			"rpc.php", "rest.php", "service.php",
			"handler.php", "gateway.php", "soap.php",
			"/bitrix/services/", "/services/main/ajax",
		},
		AssetPatterns: []string{
			"google-fonts", "fonts.googleapis", "fonts.gstatic",
			"cdnjs.cloudflare", "unpkg.com", "jsdelivr.net",
			"static.", "assets.", "/assets/", "/static/",
			".min.", ".bundle.",
		},
		AssetExtensions: []string{
			".css", ".scss", ".sass", ".less",
			".js", ".mjs", ".ts",
			".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp", ".avif",
			".woff", ".woff2", ".ttf", ".otf", ".eot",
			".mp4", ".webm", ".ogg", ".mp3", ".wav", ".flac",
			".pdf", ".doc", ".docx", ".zip", ".rar",
			".json", ".xml",
		},
		WAFPatterns: []string{
			"/cdn-cgi/",
			"cloudflare",
			"__cdn",
			"waf",
			"bot-detect",
			"recaptcha",
			"challenge",
		},
		CDNDomains: []string{
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
		},
		CachePatterns: []string{
			"?v=", "&v=", "?ver=", "&ver=", "?version=", "&version=",
			"cache-buster", "cachebuster",
		},
		Tags: map[string][]string{
			"auth":   {"/login", "/auth/", "/signin", "/signup", "/register", "/forgot", "/reset"},
			"admin":  {"/admin/", "/dashboard", "/manage", "/control", "/moder", "/panel", "/cp/"},
			"api":    {"/api/", "/rest/", "/graphql", "/ajax", "/rpc/", "/v1/", "/v2/"},
			"cdn":    {"cdn.", "cloudfront", "fastly"},
			"static": {"/assets/", "/static/", "/uploads/", "/files/", ".min.", ".bundle."},
		},
	}
}

func Bitrix() *PatternSet {
	return &PatternSet{
		APIPatterns: []string{
			"/bitrix/services/", "/bitrix/tools/",
			"/bitrix/components/bitrix/",
			"ajax.php", "rest.php",
			"/rest/", "/oauth/",
		},
		CachePatterns: []string{
			"?bitrix_include_areas=", "?bxrand=", "?sessid=",
		},
		Tags: map[string][]string{
			"bitrix-api":  {"/bitrix/services/", "/bitrix/tools/", "/rest/"},
			"bitrix-auth": {"/auth/", "/bitrix/services/main/ajax.php?action=auth", "/oauth/"},
		},
	}
}

func WordPress() *PatternSet {
	return &PatternSet{
		APIPatterns: []string{
			"/wp-json/", "/wp-admin/admin-ajax.php",
			"/xmlrpc.php", "/wp-cron.php",
			"/rest_route/",
		},
		AssetPatterns: []string{
			"/wp-content/", "/wp-includes/",
			"/wp-json/",
		},
		WAFPatterns: []string{
			"wordfence", "wpscan", "wp-login",
		},
		CachePatterns: []string{
			"?wp_cache=", "?w3tc_", "?page_id=",
		},
		Tags: map[string][]string{
			"wp-api":   {"/wp-json/", "/xmlrpc.php", "/wp-admin/admin-ajax.php"},
			"wp-auth":  {"/wp-login", "/wp-admin/"},
			"wp-admin": {"/wp-admin/"},
		},
	}
}

func ReactSPA() *PatternSet {
	return &PatternSet{
		APIPatterns: []string{
			"/api/", "/graphql", "/rest/",
		},
	}
}

func LoadFile(path string) (*PatternSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read patterns file: %w", err)
	}
	p := &PatternSet{}
	if err := json.Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("parse patterns file: %w", err)
	}
	return p, nil
}

func Merge(dest *PatternSet, srcs ...*PatternSet) *PatternSet {
	if dest == nil {
		dest = &PatternSet{}
	}
	for _, src := range srcs {
		if src == nil {
			continue
		}
		dest.APIPatterns = append(dest.APIPatterns, src.APIPatterns...)
		dest.AssetPatterns = append(dest.AssetPatterns, src.AssetPatterns...)
		dest.AssetExtensions = append(dest.AssetExtensions, src.AssetExtensions...)
		dest.WAFPatterns = append(dest.WAFPatterns, src.WAFPatterns...)
		dest.CDNDomains = append(dest.CDNDomains, src.CDNDomains...)
		dest.CachePatterns = append(dest.CachePatterns, src.CachePatterns...)
		if dest.Tags == nil {
			dest.Tags = make(map[string][]string)
		}
		for k, v := range src.Tags {
			dest.Tags[k] = append(dest.Tags[k], v...)
		}
	}
	return dest
}

func (p *PatternSet) MatchTag(rawURL string) string {
	lower := strings.ToLower(rawURL)
	for tag, patterns := range p.Tags {
		for _, pat := range patterns {
			if strings.Contains(lower, strings.ToLower(pat)) {
				return tag
			}
		}
	}
	return ""
}

func MessageOfTheDay() string {
	return strings.Join([]string{
		"Quick flags:",
		"  -bitrix      Enable Bitrix CMS patterns",
		"  -wp          Enable WordPress patterns",
		"  -react       Enable React SPA patterns",
		"  -full        Show all URL classes (WAF/CDN/Cache/Noise)",
		"  -f DOMAINS   Also crawl these comma-separated domains (e.g. -f api.site.com,cdn.site.com)",
		"  -H \"K: V\"     Send an HTTP header (curl-style, repeatable), e.g. -H \"Authorization: Bearer ...\"",
		"  -b \"k=v\"      Send a Cookie header (session cookies also persist across the crawl)",
		"  -patterns FILE  Load custom patterns from JSON file",
		"",
		"Tag system:",
		"  Tags (auth, admin, api, cdn, static) are shown when -t is set.",
		"  Custom tags can be defined in a patterns JSON file.",
		"",
		"Examples:",
		"  webmap -url https://example.com -bitrix",
		"  webmap -url https://example.com -patterns my-site.json",
		"  webmap -url https://example.com -r -rdepth 3 -j -G -react",
		"  webmap -url https://example.com -full",
	}, "\n")
}
