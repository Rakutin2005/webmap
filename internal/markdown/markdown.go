package markdown

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"apimap/internal/categorizer"
	"apimap/internal/config"
	"apimap/internal/linker"
)

type PageNode struct {
	URL      string
	Links    []linker.Link
	Children map[string]*PageNode
}

func DirectoryName(url string) string {
	u := strings.TrimPrefix(url, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.ReplaceAll(u, "/", "_")
	u = strings.ReplaceAll(u, ".", "_")
	u = strings.ReplaceAll(u, " ", "_")
	if u == "" || u == "_" {
		return "output.wmap"
	}
	if strings.HasSuffix(u, "_") {
		u = u[:len(u)-1]
	}
	return u + ".wmap"
}

func Generate(dirName string, baseURL string, links []linker.Link, stats *categorizer.Stats, cfg *config.Config, hasGraph bool) error {
	if err := os.MkdirAll(dirName, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	root := buildTree(links, baseURL)

	siteTree := generateTree(root, 0)
	readme := generateReadme(baseURL, links, stats, cfg, siteTree, hasGraph)
	if err := os.WriteFile(filepath.Join(dirName, "README.md"), []byte(readme), 0644); err != nil {
		return fmt.Errorf("failed to write README: %w", err)
	}

	pagesMD := generatePages(links, cfg)
	if err := os.WriteFile(filepath.Join(dirName, "pages.md"), []byte(pagesMD), 0644); err != nil {
		return fmt.Errorf("failed to write pages: %w", err)
	}

	apiMD := generateAPISection(links)
	if err := os.WriteFile(filepath.Join(dirName, "api.md"), []byte(apiMD), 0644); err != nil {
		return fmt.Errorf("failed to write api: %w", err)
	}

	assetsMD := generateAssets(links, cfg)
	if err := os.WriteFile(filepath.Join(dirName, "assets.md"), []byte(assetsMD), 0644); err != nil {
		return fmt.Errorf("failed to write assets: %w", err)
	}

	return nil
}

func buildTree(links []linker.Link, baseURL string) *PageNode {
	root := &PageNode{
		URL:      baseURL,
		Children: make(map[string]*PageNode),
	}

	pagesLinks := make(map[string][]linker.Link)
	for _, link := range links {
		pagesLinks[link.SourceURL] = append(pagesLinks[link.SourceURL], link)
	}

	for pageURL, pageLinks := range pagesLinks {
		filtered := filterValuableLinks(pageLinks)
		if len(filtered) > 0 {
			root.Children[pageURL] = &PageNode{
				URL:      pageURL,
				Links:    filtered,
				Children: make(map[string]*PageNode),
			}
		}
	}

	return root
}

func filterValuableLinks(links []linker.Link) []linker.Link {
	var result []linker.Link

	for _, link := range links {
		if shouldSkip(link) {
			continue
		}
		result = append(result, link)
	}

	return result
}

func shouldSkip(link linker.Link) bool {
	href := strings.ToLower(link.HREF)

	if link.Category == linker.CategoryWebAsset && !isNavigationAsset(href) {
		return true
	}

	if link.Category == linker.CategoryWebPage {
		return false
	}

	skipExts := []string{".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp", ".avif", ".css", ".scss", ".sass", ".woff", ".woff2", ".ttf", ".otf", ".eot", ".mp4", ".webm", ".ogg", ".mp3", ".wav", ".flac", ".pdf", ".doc", ".docx", ".zip"}

	for _, ext := range skipExts {
		if strings.HasSuffix(href, ext) {
			return true
		}
	}

	return false
}

func isNavigationAsset(href string) bool {
	navIndicators := []string{"/api/", "/v1/", "/v2/", "endpoint", "/ajax", "/xhr", "graphql", "/rest/", "_api", ".json", ".xml"}
	for _, ind := range navIndicators {
		if strings.Contains(href, ind) {
			return true
		}
	}
	return false
}

func generateTree(node *PageNode, depth int) string {
	var sb strings.Builder

	groupedLinks := groupByCategory(node.Links)

	pagesLinks := groupedLinks[linker.CategoryWebPage]
	apiLinks := groupedLinks[linker.CategoryAPI]
	assetLinks := groupedLinks[linker.CategoryWebAsset]

	if len(pagesLinks) > 0 {
		if depth == 0 {
			sb.WriteString("## Pages\n\n")
		} else {
			sb.WriteString("### Pages\n\n")
		}
		for _, link := range pagesLinks {
			sb.WriteString(fmt.Sprintf("- [%s](%s) `%s`\n", getDisplayName(link.HREF), link.HREF, link.LinkType.String()))
		}
		sb.WriteString("\n")
	}

	if len(apiLinks) > 0 {
		if depth == 0 {
			sb.WriteString("## API Endpoints\n\n")
		} else {
			sb.WriteString("### API Endpoints\n\n")
		}
		for _, link := range apiLinks {
			sb.WriteString(fmt.Sprintf("- [%s](%s)\n", link.HREF, link.HREF))
		}
		sb.WriteString("\n")
	}

	if len(assetLinks) > 0 && depth == 0 {
		sb.WriteString("## Assets\n\n")
		for _, link := range assetLinks {
			sb.WriteString(fmt.Sprintf("- %s\n", link.HREF))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func groupByCategory(links []linker.Link) map[linker.Category][]linker.Link {
	grouped := make(map[linker.Category][]linker.Link)
	for _, link := range links {
		grouped[link.Category] = append(grouped[link.Category], link)
	}
	return grouped
}

func getDisplayName(url string) string {
	name := filepath.Base(url)
	if name == "/" || name == "." {
		parts := strings.Split(url, "/")
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] != "" {
				name = parts[i]
				break
			}
		}
	}
	if name == "/" {
		name = "index"
	}
	return name
}

func generateReadme(baseURL string, links []linker.Link, stats *categorizer.Stats, cfg *config.Config, siteTree string, hasGraph bool) string {
	var sb strings.Builder

	sb.WriteString("# WebMap Site Analysis\n\n")
	sb.WriteString(fmt.Sprintf("**Target URL**: `%s`\n\n", baseURL))
	sb.WriteString(fmt.Sprintf("**Generated**: %s\n\n", time.Now().Format(time.RFC3339)))

	sb.WriteString("## Summary\n\n")
	sb.WriteString(fmt.Sprintf("| Metric | Value |\n"))
	sb.WriteString(fmt.Sprintf("|--------|-------|\n"))
	sb.WriteString(fmt.Sprintf("| Total Endpoints | %d |\n", stats.Total))
	sb.WriteString(fmt.Sprintf("| Pages | %d |\n", stats.ByCategory[linker.CategoryWebPage]))
	sb.WriteString(fmt.Sprintf("| APIs | %d |\n", stats.ByCategory[linker.CategoryAPI]))
	sb.WriteString(fmt.Sprintf("| Assets | %d |\n", stats.ByCategory[linker.CategoryWebAsset]))
	sb.WriteString(fmt.Sprintf("| With Query Params | %d |\n\n", stats.WithParams))

	if hasGraph {
		sb.WriteString("## Site Graph\n\n")
		sb.WriteString("Interactive graph available: [graph.wmap/](graph.wmap/index.html)\n")
		sb.WriteString("\nOpen in a browser to explore nodes, edges, and relationships. The graph is split into multiple views to handle large datasets.\n\n")
	}

	sb.WriteString("## Site Structure\n\n")
	sb.WriteString(siteTree)

	sb.WriteString("## Quick Links\n\n")
	sb.WriteString("- [Pages](pages.md)\n")
	sb.WriteString("- [API Endpoints](api.md)\n")
	sb.WriteString("- [Assets](assets.md)\n")
	if hasGraph {
		sb.WriteString("- [Interactive Graph](graph.wmap/index.html)\n")
	}

	return sb.String()
}

func generatePages(links []linker.Link, cfg *config.Config) string {
	var sb strings.Builder
	sb.WriteString("# Pages\n\n")

	pagesLinks := make(map[string][]linker.Link)
	for _, link := range links {
		if link.Category == linker.CategoryWebPage {
			pagesLinks[link.SourceURL] = append(pagesLinks[link.SourceURL], link)
		}
	}

	var sortedPages []string
	for url := range pagesLinks {
		sortedPages = append(sortedPages, url)
	}
	sort.Strings(sortedPages)

	for _, pageURL := range sortedPages {
		pageLinks := pagesLinks[pageURL]
		if len(pageLinks) == 0 {
			continue
		}

		sb.WriteString("## " + getDisplayName(pageURL) + "\n\n")
		sb.WriteString("Source: `" + pageURL + "`\n\n")
		sb.WriteString("| Link | Category | Type |\n")
		sb.WriteString("|------|----------|------|\n")

		for _, link := range pageLinks {
			sb.WriteString("| " + link.HREF + " | " + link.Category.String() + " | " + link.LinkType.String() + " |\n")
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func generateAPISection(links []linker.Link) string {
	var sb strings.Builder
	sb.WriteString("# API Endpoints\n\n")

	var apis []linker.Link
	for _, link := range links {
		if link.Category == linker.CategoryAPI {
			apis = append(apis, link)
		}
	}

	if len(apis) == 0 {
		sb.WriteString("No API endpoints found.\n")
		return sb.String()
	}

	grouped := make(map[string][]linker.Link)
	for _, api := range apis {
		path := extractAPIPath(api.HREF)
		grouped[path] = append(grouped[path], api)
	}

	var paths []string
	for p := range grouped {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, apiPath := range paths {
		sb.WriteString("## " + apiPath + "\n\n")

		seen := make(map[string]bool)
		for _, api := range grouped[apiPath] {
			if !seen[api.HREF] {
				seen[api.HREF] = true
				sb.WriteString("- **URL**: `" + api.HREF + "`\n")
				sb.WriteString("  - Source: " + api.SourceURL + "\n")
				sb.WriteString("  - Type: " + api.LinkType.String() + "\n")
				if api.Tag != "" {
					sb.WriteString("  - Found in: " + api.Tag + "\n")
				}
				sb.WriteString("\n")
			}
		}
	}

	return sb.String()
}

func extractAPIPath(href string) string {
	parts := strings.Split(href, "/")
	if len(parts) > 2 && strings.Contains(parts[2], "api") {
		return "/" + strings.Join(parts[2:len(parts)-1], "/")
	}
	return "/" + filepath.Base(href)
}

func generateAssets(links []linker.Link, cfg *config.Config) string {
	var sb strings.Builder
	sb.WriteString("# Assets\n\n")

	var assets []linker.Link
	for _, link := range links {
		if link.Category == linker.CategoryWebAsset && !shouldSkip(link) {
			assets = append(assets, link)
		}
	}

	if len(assets) == 0 {
		sb.WriteString("No significant assets found.\n")
		return sb.String()
	}

	sb.WriteString("| Asset | Source |\n")
	sb.WriteString("|-------|-------|\n")

	for _, asset := range assets {
		sb.WriteString(fmt.Sprintf("| %s | %s |\n", asset.HREF, asset.SourceURL))
	}

	return sb.String()
}
