package color

import (
	"fmt"
	"strings"
)

type Code string

const (
	Reset Code = "\033[0m"
	Bold  Code = "\033[1m"
	Dim   Code = "\033[2m"

	Red    Code = "\033[31m"
	Green  Code = "\033[32m"
	Yellow Code = "\033[33m"
	Blue   Code = "\033[34m"
	Purple Code = "\033[35m"
	Cyan   Code = "\033[36m"
	Gray   Code = "\033[37m"

	DarkGray   Code = "\033[90m"
	DarkRed    Code = "\033[91m"
	DarkGreen  Code = "\033[92m"
	DarkYellow Code = "\033[93m"
	DarkBlue   Code = "\033[94m"
	DarkPurple Code = "\033[95m"
	DarkCyan   Code = "\033[96m"
	White      Code = "\033[97m"
)

var wellKnownDomains = []string{
	"cloudflare", "cloudflareinsights",
	"googleapis", "googletagmanager", "google-analytics", "google", "gstatic", "youtube", "ytimg",
	"github", "githubusercontent",
	"facebook", "fbcdn",
	"twitter", "twimg", "x.com",
	"linkedin",
	"jsdelivr", "cdnjs", "unpkg", "bootstrapcdn", "jquery",
	"fontawesome", "fonts.gstatic", "fonts.googleapis",
	"amazonaws", "aws",
	"microsoft", "azure",
	"vimeo", "instagram", "tiktok",
	"yandex", "mail.ru", "vk",
}

type DomainType int

const (
	DomainTarget DomainType = iota
	DomainSubdomain
	DomainWellKnown
	DomainOther
)

func DomainColor(dt DomainType) Code {
	switch dt {
	case DomainTarget:
		return Green
	case DomainSubdomain:
		return DarkYellow
	case DomainWellKnown:
		return Red
	default:
		return Gray
	}
}

func ClassifyDomain(host, targetHost string) DomainType {
	if host == "" {
		return DomainOther
	}
	if host == targetHost {
		return DomainTarget
	}
	if strings.HasSuffix(host, "."+targetHost) {
		return DomainSubdomain
	}
	lower := strings.ToLower(host)
	for _, wkd := range wellKnownDomains {
		if strings.Contains(lower, wkd) {
			return DomainWellKnown
		}
	}
	return DomainOther
}

func DomainLabel(dt DomainType) string {
	switch dt {
	case DomainTarget:
		return "target"
	case DomainSubdomain:
		return "subdomain"
	case DomainWellKnown:
		return "well-known"
	default:
		return "other"
	}
}

type AssetSubtype int

const (
	AssetJS AssetSubtype = iota
	AssetCSS
	AssetImage
	AssetFont
	AssetDoc
	AssetMedia
	AssetData
	AssetOther
)

func ClassifyAsset(href string) AssetSubtype {
	lower := strings.ToLower(href)
	switch {
	case strings.HasSuffix(lower, ".js") || strings.HasSuffix(lower, ".mjs") || strings.HasSuffix(lower, ".cjs") || strings.HasSuffix(lower, ".jsx") || strings.HasSuffix(lower, ".ts") || strings.HasSuffix(lower, ".tsx") || strings.HasSuffix(lower, ".mts") || strings.HasSuffix(lower, ".cts"):
		return AssetJS
	case strings.HasSuffix(lower, ".css") || strings.HasSuffix(lower, ".scss") || strings.HasSuffix(lower, ".sass") || strings.HasSuffix(lower, ".less"):
		return AssetCSS
	case strings.HasSuffix(lower, ".png") || strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg") || strings.HasSuffix(lower, ".gif") || strings.HasSuffix(lower, ".svg") || strings.HasSuffix(lower, ".ico") || strings.HasSuffix(lower, ".webp") || strings.HasSuffix(lower, ".avif"):
		return AssetImage
	case strings.HasSuffix(lower, ".woff") || strings.HasSuffix(lower, ".woff2") || strings.HasSuffix(lower, ".ttf") || strings.HasSuffix(lower, ".otf") || strings.HasSuffix(lower, ".eot"):
		return AssetFont
	case strings.HasSuffix(lower, ".pdf") || strings.HasSuffix(lower, ".doc") || strings.HasSuffix(lower, ".docx") || strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".rar"):
		return AssetDoc
	case strings.HasSuffix(lower, ".mp4") || strings.HasSuffix(lower, ".webm") || strings.HasSuffix(lower, ".ogg") || strings.HasSuffix(lower, ".mp3") || strings.HasSuffix(lower, ".wav") || strings.HasSuffix(lower, ".flac"):
		return AssetMedia
	case strings.HasSuffix(lower, ".json") || strings.HasSuffix(lower, ".xml"):
		return AssetData
	default:
		return AssetOther
	}
}

func AssetColor(as AssetSubtype) Code {
	switch as {
	case AssetJS:
		return Yellow
	case AssetCSS:
		return Blue
	case AssetImage:
		return Purple
	case AssetFont:
		return DarkGray
	case AssetDoc:
		return White
	case AssetMedia:
		return Cyan
	case AssetData:
		return DarkYellow
	default:
		return White
	}
}

func AssetLabel(as AssetSubtype) string {
	switch as {
	case AssetJS:
		return "js"
	case AssetCSS:
		return "css"
	case AssetImage:
		return "img"
	case AssetFont:
		return "font"
	case AssetDoc:
		return "doc"
	case AssetMedia:
		return "media"
	case AssetData:
		return "data"
	default:
		return "asset"
	}
}

func Colorize(c Code, s string) string {
	return string(c) + s + string(Reset)
}

func Colorizef(c Code, format string, a ...interface{}) string {
	return string(c) + fmt.Sprintf(format, a...) + string(Reset)
}
