package graph

import (
	"apimap/internal/linker"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type NodeKind int

const (
	KindRoot NodeKind = iota
	KindWebPage
	KindAPIEndpoint
	KindAPIFile
	KindJSAsset
	KindCSSAsset
	KindImageAsset
	KindFontAsset
	KindMediaAsset
	KindDataAsset
	KindDocAsset
	KindOtherAsset
	KindWAF
	KindCDN
	KindCache
	KindNoise
)

type Graph struct {
	links     []linker.Link
	rootURL   string
	ShowWAF   bool
	ShowCDN   bool
	ShowCache bool
	ShowNoise bool
}

func New(links []linker.Link, rootURL string) *Graph {
	return &Graph{links: links, rootURL: rootURL}
}

type visNode struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Group     string `json:"group"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	KindLabel string `json:"kindLabel"`
	Params    string `json:"params,omitempty"`
}

type visEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Arrows string `json:"arrows"`
	Title  string `json:"title"`
}

type graphData struct {
	Nodes   []visNode           `json:"nodes"`
	Edges   []visEdge           `json:"edges"`
	Sources map[string][]string `json:"sources"`
	Stats   struct {
		TotalNodes int            `json:"totalNodes"`
		TotalEdges int            `json:"totalEdges"`
		ByGroup    map[string]int `json:"byGroup"`
	} `json:"stats"`
}

// --- helpers ---

func sanitizeID(url string) string {
	s := strings.ReplaceAll(url, "://", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, ".", "_")
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, "?", "_")
	s = strings.ReplaceAll(s, "&", "_")
	s = strings.ReplaceAll(s, "=", "_")
	s = strings.ReplaceAll(s, "%", "_")
	s = strings.ReplaceAll(s, "#", "_")
	s = strings.ReplaceAll(s, "'", "_")
	return "n" + s
}

func shortLabel(url string) string {
	u := strings.TrimPrefix(url, "https://")
	u = strings.TrimPrefix(u, "http://")
	if len(u) <= 35 {
		return u
	}
	return u[:32] + "..."
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- classification ---

func classifyAssetKind(href string) NodeKind {
	lower := strings.ToLower(href)
	switch {
	case strings.HasSuffix(lower, ".js") || strings.HasSuffix(lower, ".mjs") || strings.HasSuffix(lower, ".cjs") || strings.HasSuffix(lower, ".jsx") || strings.HasSuffix(lower, ".ts") || strings.HasSuffix(lower, ".tsx") || strings.HasSuffix(lower, ".mts") || strings.HasSuffix(lower, ".cts"):
		return KindJSAsset
	case strings.HasSuffix(lower, ".css") || strings.HasSuffix(lower, ".scss") || strings.HasSuffix(lower, ".sass") || strings.HasSuffix(lower, ".less"):
		return KindCSSAsset
	case strings.HasSuffix(lower, ".png") || strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg") || strings.HasSuffix(lower, ".gif") || strings.HasSuffix(lower, ".svg") || strings.HasSuffix(lower, ".ico") || strings.HasSuffix(lower, ".webp") || strings.HasSuffix(lower, ".avif"):
		return KindImageAsset
	case strings.HasSuffix(lower, ".woff") || strings.HasSuffix(lower, ".woff2") || strings.HasSuffix(lower, ".ttf") || strings.HasSuffix(lower, ".otf") || strings.HasSuffix(lower, ".eot"):
		return KindFontAsset
	case strings.HasSuffix(lower, ".mp4") || strings.HasSuffix(lower, ".webm") || strings.HasSuffix(lower, ".ogg") || strings.HasSuffix(lower, ".mp3") || strings.HasSuffix(lower, ".wav") || strings.HasSuffix(lower, ".flac"):
		return KindMediaAsset
	case strings.HasSuffix(lower, ".pdf") || strings.HasSuffix(lower, ".doc") || strings.HasSuffix(lower, ".docx") || strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".rar"):
		return KindDocAsset
	case strings.HasSuffix(lower, ".json") || strings.HasSuffix(lower, ".xml"):
		return KindDataAsset
	default:
		return KindOtherAsset
	}
}

func determineKind(url string, cats map[linker.Category]bool, hasClass linker.URLClass, isAPISource bool) NodeKind {
	switch hasClass {
	case linker.ClassWAF:
		return KindWAF
	case linker.ClassCDN:
		return KindCDN
	case linker.ClassCache:
		return KindCache
	case linker.ClassNoise:
		return KindNoise
	}
	if isAPISource {
		return KindAPIFile
	}
	if cats[linker.CategoryAPI] {
		return KindAPIEndpoint
	}
	if cats[linker.CategoryWebPage] {
		return KindWebPage
	}
	if cats[linker.CategoryWebAsset] {
		return classifyAssetKind(url)
	}
	return KindOtherAsset
}

func (nk NodeKind) GroupName() string {
	names := map[NodeKind]string{
		KindRoot: "root", KindWebPage: "webpage", KindAPIEndpoint: "api",
		KindAPIFile: "apifile", KindJSAsset: "js", KindCSSAsset: "css",
		KindImageAsset: "image", KindFontAsset: "font", KindMediaAsset: "media",
		KindDataAsset: "data", KindDocAsset: "doc", KindOtherAsset: "other",
		KindWAF: "waf", KindCDN: "cdn", KindCache: "cache", KindNoise: "noise",
	}
	if n, ok := names[nk]; ok {
		return n
	}
	return "other"
}

func (nk NodeKind) Shape() string {
	m := map[NodeKind]string{
		KindRoot: "star", KindWebPage: "box", KindAPIEndpoint: "diamond",
		KindAPIFile: "square", KindJSAsset: "ellipse", KindCSSAsset: "box",
		KindImageAsset: "hexagon", KindFontAsset: "hexagon", KindMediaAsset: "triangle",
		KindDataAsset: "diamond", KindDocAsset: "box", KindOtherAsset: "ellipse",
		KindWAF: "box", KindCDN: "box", KindCache: "box", KindNoise: "ellipse",
	}
	if s, ok := m[nk]; ok {
		return s
	}
	return "ellipse"
}

func (nk NodeKind) KindLabel() string {
	m := map[NodeKind]string{
		KindRoot: "Root (target URL)", KindWebPage: "Web Page",
		KindAPIEndpoint: "API Endpoint", KindAPIFile: "JS File (with API calls)",
		KindJSAsset: "JavaScript", KindCSSAsset: "CSS Stylesheet",
		KindImageAsset: "Image", KindFontAsset: "Font",
		KindMediaAsset: "Media", KindDataAsset: "Data (JSON/XML)",
		KindDocAsset: "Document", KindOtherAsset: "Asset (other)",
		KindWAF: "WAF / Cloudflare", KindCDN: "CDN",
		KindCache: "Cache", KindNoise: "Noise (possible non-URL)",
	}
	if l, ok := m[nk]; ok {
		return l
	}
	return "Unknown"
}

// --- build graph data ---

func (g *Graph) build() *graphData {
	type urlInfo struct {
		kind   NodeKind
		label  string
		url    string
		params string
	}

	targetEdges := make(map[string]map[string]bool)
	sourceHasAPI := make(map[string]bool)
	urlCats := make(map[string]map[linker.Category]bool)
	urlClass := make(map[string]linker.URLClass)
	urlParams := make(map[string][]string)
	baseURLs := make(map[string]string) // resolved -> base

	baseKey := func(rawURL string) string {
		idx := strings.IndexByte(rawURL, '?')
		if idx < 0 {
			return rawURL
		}
		return rawURL[:idx]
	}

	isHidden := func(u string) bool {
		cls := urlClass[u]
		switch cls {
		case linker.ClassWAF:
			return !g.ShowWAF
		case linker.ClassCDN:
			return !g.ShowCDN
		case linker.ClassCache:
			return !g.ShowCache
		case linker.ClassNoise:
			return !g.ShowNoise
		}
		return false
	}

	for _, link := range g.links {
		resolved := link.Resolved
		if resolved == "" {
			resolved = link.HREF
		}
		base := baseKey(resolved)
		baseURLs[resolved] = base
		if urlCats[base] == nil {
			urlCats[base] = make(map[linker.Category]bool)
		}
		urlCats[base][link.Category] = true
		if link.Class != linker.ClassNormal {
			urlClass[base] = link.Class
		}
		if link.HasParams || link.ParamVariants != nil {
			queryPart := ""
			if idx := strings.IndexByte(resolved, '?'); idx >= 0 {
				queryPart = resolved[idx:]
			}
			if queryPart != "" {
				urlParams[base] = append(urlParams[base], queryPart)
			}
		}
		if link.Category == linker.CategoryAPI && link.SourceURL != "" {
			sourceHasAPI[base] = true
		}
	}

	for _, link := range g.links {
		resolved := link.Resolved
		if resolved == "" {
			resolved = link.HREF
		}
		base := baseKey(resolved)
		source := link.SourceURL
		sourceBase := baseKey(source)
		if source != "" && sourceBase != base && !isHidden(base) {
			if targetEdges[sourceBase] == nil {
				targetEdges[sourceBase] = make(map[string]bool)
			}
			targetEdges[sourceBase][base] = true
		}
	}

	allURLs := make(map[string]bool)
	allURLs[g.rootURL] = true
	for _, link := range g.links {
		resolved := link.Resolved
		if resolved == "" {
			resolved = link.HREF
		}
		base := baseKey(resolved)
		if base != "" && !isHidden(base) {
			allURLs[base] = true
		}
		src := link.SourceURL
		if src != "" {
			allURLs[baseKey(src)] = true
		}
	}

	urlsSorted := make([]string, 0, len(allURLs))
	for u := range allURLs {
		urlsSorted = append(urlsSorted, u)
	}
	sort.Strings(urlsSorted)

	known := make(map[string]*urlInfo)
	for _, u := range urlsSorted {
		kind := determineKind(u, urlCats[u], urlClass[u], sourceHasAPI[u])
		label := shortLabel(u)
		paramStr := ""
		if qs, ok := urlParams[u]; ok && len(qs) > 0 {
			label += " (params)"
			paramStr = collectParams(qs)
		}
		if u == g.rootURL {
			kind = KindRoot
			label = strings.TrimPrefix(strings.TrimPrefix(g.rootURL, "https://"), "http://")
		}
		known[u] = &urlInfo{kind: kind, label: label, url: u, params: paramStr}
	}

	nodeIDs := make(map[string]string)
	var nodes []visNode
	for _, u := range urlsSorted {
		info := known[u]
		id := sanitizeID(u)
		nodeIDs[u] = id
		title := fmt.Sprintf("<strong>%s</strong><br><small>%s</small><br><em>%s</em>", info.label, info.kind.KindLabel(), info.url)
		if info.params != "" {
			title += fmt.Sprintf("<br><strong>Params:</strong> %s", info.params)
		}
		nodes = append(nodes, visNode{
			ID:        id,
			Label:     info.label,
			Group:     info.kind.GroupName(),
			Title:     title,
			URL:       info.url,
			KindLabel: info.kind.KindLabel(),
			Params:    info.params,
		})
	}

	edgeWritten := make(map[string]bool)
	var edges []visEdge
	for _, from := range urlsSorted {
		toSet := targetEdges[from]
		if len(toSet) == 0 {
			continue
		}
		fromID := nodeIDs[from]
		toKeys := make([]string, 0, len(toSet))
		for t := range toSet {
			toKeys = append(toKeys, t)
		}
		sort.Strings(toKeys)
		for _, to := range toKeys {
			toID := nodeIDs[to]
			eKey := fromID + "->" + toID
			if edgeWritten[eKey] {
				continue
			}
			edgeWritten[eKey] = true
			edges = append(edges, visEdge{
				From:   fromID,
				To:     toID,
				Arrows: "to",
				Title:  fmt.Sprintf("%s<br>↓<br>%s", known[from].label, known[to].label),
			})
		}
	}

	sources := make(map[string][]string)
	for _, link := range g.links {
		resolved := link.Resolved
		if resolved == "" {
			resolved = link.HREF
		}
		base := baseKey(resolved)
		if isHidden(base) {
			continue
		}
		src := link.SourceURL
		if src != "" && src != resolved {
			srcBase := baseKey(src)
			rid := sanitizeID(base)
			found := false
			for _, existing := range sources[srcBase] {
				if existing == rid {
					found = true
					break
				}
			}
			if !found {
				sources[srcBase] = append(sources[srcBase], rid)
			}
		}
	}

	d := &graphData{Nodes: nodes, Edges: edges, Sources: sources}
	d.Stats.TotalNodes = len(nodes)
	d.Stats.TotalEdges = len(edges)
	d.Stats.ByGroup = make(map[string]int)
	for _, n := range nodes {
		d.Stats.ByGroup[n.Group]++
	}
	return d
}

// --- group definitions ---

const groupDefs = `
  root:{shape:'star',color:{background:'#e1f5fe',border:'#01579b'},font:{color:'#01579b',size:18},borderWidth:3,size:36},
  webpage:{shape:'box',color:{background:'#e8f5e9',border:'#2e7d32'},font:{color:'#1b5e20'},borderWidth:2},
  api:{shape:'diamond',color:{background:'#fff3e0',border:'#e65100'},font:{color:'#bf360c'},borderWidth:2},
  apifile:{shape:'square',color:{background:'#e3f2fd',border:'#1565c0'},font:{color:'#0d47a1'},shapeProperties:{borderDashes:[5,5]}},
  js:{shape:'ellipse',color:{background:'#fff8e1',border:'#f57f17'},font:{color:'#e65100'}},
  css:{shape:'box',color:{background:'#e8eaf6',border:'#283593'},font:{color:'#1a237e'}},
  image:{shape:'hexagon',color:{background:'#f3e5f5',border:'#6a1b9a'},font:{color:'#4a148c'}},
  font:{shape:'hexagon',color:{background:'#eceff1',border:'#546e7a'},font:{color:'#37474f'}},
  media:{shape:'triangle',color:{background:'#e0f7fa',border:'#00838f'},font:{color:'#006064'}},
  data:{shape:'diamond',color:{background:'#fce4ec',border:'#c62828'},font:{color:'#b71c1c'}},
  doc:{shape:'box',color:{background:'#efebe9',border:'#4e342e'},font:{color:'#3e2723'}},
  other:{shape:'ellipse',color:{background:'#f5f5f5',border:'#9e9e9e'},font:{color:'#616161',italic:true}},
  waf:{shape:'box',color:{background:'#ffebee',border:'#b71c1c'},font:{color:'#c62828'},borderWidth:2,shapeProperties:{borderDashes:[5,3]}},
  cdn:{shape:'box',color:{background:'#fff3e0',border:'#e65100'},font:{color:'#e65100'},shapeProperties:{borderDashes:[5,5]}},
  cache:{shape:'box',color:{background:'#f1f8e9',border:'#558b2f'},font:{color:'#33691e'},shapeProperties:{borderDashes:[5,5]}},
  noise:{shape:'ellipse',color:{background:'#fafafa',border:'#bdbdbd'},font:{color:'#9e9e9e',italic:true},shapeProperties:{borderDashes:[3,3]}},
`

// --- HTML templates ---

func visLibrary() string {
	return `<script src="https://cdnjs.cloudflare.com/ajax/libs/vis/4.21.0/vis.min.js"></script><link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/vis/4.21.0/vis.min.css">`
}

func legendBlock() string {
	items := []struct{ cls, label, color string }{
		{"root", "Root (target URL)", "#e1f5fe"},
		{"webpage", "Web Page", "#e8f5e9"}, {"api", "API Endpoint", "#fff3e0"},
		{"apifile", "JS File (with API)", "#e3f2fd"}, {"js", "JavaScript", "#fff8e1"},
		{"css", "CSS", "#e8eaf6"}, {"image", "Image", "#f3e5f5"},
		{"font", "Font", "#eceff1"}, {"media", "Media", "#e0f7fa"},
		{"data", "Data (JSON/XML)", "#fce4ec"}, {"doc", "Document", "#efebe9"},
		{"waf", "WAF / Cloudflare", "#ffebee"}, {"cdn", "CDN", "#fff3e0"},
		{"cache", "Cache", "#f1f8e9"}, {"noise", "Noise (possible non-URL)", "#fafafa"},
	}
	var sb strings.Builder
	for _, it := range items {
		sb.WriteString(fmt.Sprintf(
			`<span class="lg-item" data-group="%s" style="display:inline-block;margin:2px 10px 2px 0;white-space:nowrap;font-size:12px;cursor:pointer" onclick="toggleGroup('%s')"><span style="display:inline-block;width:12px;height:12px;background:%s;border:1px solid #888;vertical-align:middle;margin-right:4px;border-radius:2px"></span>%s</span>`,
			it.cls, it.cls, it.color, it.label))
	}
	return sb.String()
}

func pageNav(title string) string {
	return fmt.Sprintf(`<div style="background:#f5f5f5;border-bottom:1px solid #ddd;padding:8px 16px;font-size:13px"><strong>%s</strong> &nbsp;·&nbsp; <a href="index.html">Hub</a> &nbsp;·&nbsp; <a href="full.html">Full Graph</a> &nbsp;·&nbsp; <a href="sources.html">By Source</a></div>`, title)
}

// --- render functions ---

func renderIndex(rootURL string, d *graphData) []byte {
	var byGroupRows strings.Builder
	order := []string{"root", "webpage", "api", "apifile", "js", "css", "image", "font", "media", "data", "doc", "waf", "cdn", "cache", "noise", "other"}
	for _, g := range order {
		if n := d.Stats.ByGroup[g]; n > 0 {
			byGroupRows.WriteString(fmt.Sprintf("<tr><td>%s</td><td>%d</td></tr>\n", g, n))
		}
	}

	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>WebMap Graph — %s</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif;color:#222;padding:24px;background:#fafafa}
h1{font-size:22px;margin-bottom:4px}
h2{font-size:16px;margin:20px 0 8px;color:#555}
p,li{font-size:14px;line-height:1.6}
a{color:#1565c0;text-decoration:none}
a:hover{text-decoration:underline}
table{border-collapse:collapse;font-size:13px}
td,th{border:1px solid #ddd;padding:4px 10px;text-align:left}
th{background:#f0f0f0}
.grid{display:flex;flex-wrap:wrap;gap:8px}
.card{background:#fff;border:1px solid #ddd;border-radius:6px;padding:14px 18px;flex:1 1 280px;box-shadow:0 1px 3px rgba(0,0,0,.06)}
.card h3{font-size:14px;margin-bottom:6px}
.card .num{font-size:28px;font-weight:700;color:#1565c0}
.legend{margin:12px 0;padding:10px 14px;background:#fff;border:1px solid #ddd;border-radius:6px;font-size:12px;line-height:1.8}
.actions{margin:16px 0}
.btn{display:inline-block;padding:10px 20px;margin:0 8px 8px 0;background:#1565c0;color:#fff;border-radius:6px;font-size:14px;font-weight:600}
.btn:hover{background:#0d47a1;text-decoration:none}
</style></head><body>
<h1>WebMap Graph</h1>
<p style="color:#666">Target: <code>%s</code></p>

<div class="legend"><strong>Legend</strong><br>%s</div>

<div class="grid">
  <div class="card"><h3>Total Nodes</h3><div class="num">%d</div></div>
  <div class="card"><h3>Edges</h3><div class="num">%d</div></div>
  <div class="card"><h3>Source Pages</h3><div class="num">%d</div></div>
</div>

<div class="actions">
  <a class="btn" href="full.html">Open Full Graph</a>
  <a class="btn" href="sources.html">Browse by Source</a>
</div>

<h2>Nodes by Type</h2>
<table><tr><th>Type</th><th>Count</th></tr>%s</table>

<h2>Source Pages</h2>
<ul>%s</ul>

<p style="margin-top:20px;font-size:12px;color:#999">Generated by WebMap &mdash; <a href="data.json">data.json</a></p>
</body></html>`,
		jsString(rootURL), jsString(rootURL),
		legendBlock(),
		d.Stats.TotalNodes, d.Stats.TotalEdges, len(d.Sources),
		byGroupRows.String(),
		sourceList(d))

	return []byte(html)
}

func sourceList(d *graphData) string {
	srcKeys := make([]string, 0, len(d.Sources))
	for s := range d.Sources {
		srcKeys = append(srcKeys, s)
	}
	sort.Strings(srcKeys)
	var sb strings.Builder
	for _, s := range srcKeys {
		ids := d.Sources[s]
		sb.WriteString(fmt.Sprintf("<li><a href=\"sources.html#%s\">%s</a> (%d links)</li>\n",
			sanitizeID(s), s, len(ids)))
	}
	return sb.String()
}

func renderFull(rootURL string, d *graphData) []byte {
	nj, _ := json.Marshal(d.Nodes)
	ej, _ := json.Marshal(d.Edges)

	return []byte(fmt.Sprintf(`<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>Full Graph — %s</title>%s
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif;overflow:hidden;background:#fff}
#mynetwork{width:100%%;height:100vh}
#toolbar{position:fixed;top:12px;right:12px;z-index:1000;display:flex;flex-direction:column;gap:4px}
#toolbar button,#toolbar a{width:40px;height:36px;font-size:16px;cursor:pointer;background:#fff;border:1px solid #ccc;border-radius:4px;box-shadow:0 1px 3px rgba(0,0,0,.12);display:flex;align-items:center;justify-content:center;text-decoration:none;color:#333}
#toolbar button:hover,#toolbar a:hover{background:#f0f0f0}
#info{position:fixed;bottom:12px;left:12px;right:12px;z-index:1000;background:rgba(255,255,255,.95);border:1px solid #ccc;border-radius:6px;padding:12px 16px;font-size:13px;line-height:1.5;display:none;max-height:150px;overflow-y:auto;box-shadow:0 -1px 6px rgba(0,0,0,.1)}
#info .close{float:right;cursor:pointer;font-size:18px;color:#999;background:none;border:none;padding:0 4px}
#info .close:hover{color:#333}
#info strong{color:#222}
#info .url{color:#1565c0;word-break:break-all}
#loading{position:fixed;top:50%%;left:50%%;transform:translate(-50%%,-50%%);z-index:999;background:rgba(255,255,255,.9);padding:20px 30px;border-radius:8px;border:1px solid #ddd;font-size:14px;text-align:center}
#legend{position:fixed;top:12px;left:12px;z-index:1000;background:rgba(255,255,255,.93);border:1px solid #ccc;border-radius:6px;padding:8px 12px;font-size:11px;line-height:1.7;max-width:480px;box-shadow:0 1px 4px rgba(0,0,0,.1);display:none}
#legend .lg-item{display:inline-block;margin:0 6px 0 0;white-space:nowrap}
#legend .lg-dot{display:inline-block;width:10px;height:10px;border:1px solid #888;vertical-align:middle;margin-right:3px;border-radius:2px}
#legendToggle{position:fixed;top:14px;left:14px;z-index:1001;background:#fff;border:1px solid #ccc;border-radius:4px;padding:4px 10px;font-size:12px;cursor:pointer;box-shadow:0 1px 3px rgba(0,0,0,.12)}
#legendToggle:hover{background:#f0f0f0}
</style></head><body>
<div id="legendToggle" onclick="toggleLegend()">Legend</div>
<div id="legend">%s</div>
<div id="loading">Loading graph (%d nodes, %d edges)...</div>
<div id="toolbar">
<button onclick="network.zoomIn()" title="Zoom in">+</button>
<button onclick="network.zoomOut()" title="Zoom out">−</button>
<button onclick="network.fit()" title="Fit">⊞</button>
<button onclick="togglePhysics()" title="Toggle physics">⟳</button>
<a href="index.html" title="Hub">⌂</a>
</div>
<div id="mynetwork"></div>
<div id="info"><button class="close" onclick="closeInfo()">&times;</button><div id="infoContent"></div></div>
<script>
var allNodes = %s;
var allEdges = %s;
var groups = {%s};
var activeGroups = {};
allNodes.forEach(function(n){activeGroups[n.group]=true});
var nodes = new vis.DataSet(allNodes);
var edges = new vis.DataSet(allEdges);
var container = document.getElementById('mynetwork');
var options = {
  groups:groups,
  nodes:{font:{face:'monospace',size:12},margin:8,shapeProperties:{borderRadius:4}},
  edges:{smooth:{type:'curvedCW',roundness:0.12},color:{color:'#888',highlight:'#e65100'},font:{size:10,face:'monospace'}},
  physics:{enabled:true,solver:'barnesHut',barnesHut:{gravitationalConstant:-8000,centralGravity:0.15,springLength:220,springConstant:0.02,damping:0.08},stabilization:{iterations:50,updateInterval:10}},
  interaction:{hover:true,tooltipDelay:80,keyboard:true,navigationButtons:true},
  layout:{improvedLayout:true,randomSeed:42}
};
var network = new vis.Network(container,{nodes:nodes,edges:edges},options);
network.once('stableIteration',function(){document.getElementById('loading').style.display='none'});
network.on('click',function(p){
  var el=document.getElementById('info'),ct=document.getElementById('infoContent');
  if(p.nodes.length>0){var n=nodes.get(p.nodes[0]);if(n){var h='<strong>'+(n.kindLabel||'Node')+'</strong><br><span class=\"url\">'+(n.url||n.label)+'</span>';if(n.params){h+='<br><strong>Params:</strong> <span style=\"color:#e65100\">'+n.params+'</span>'}h+='<br><em style=\"color:#666\">Click outside to close</em>';ct.innerHTML=h;el.style.display='block'}}
  else if(p.edges.length>0){var e=edges.get(p.edges[0]);if(e){var fn=nodes.get(e.from),tn=nodes.get(e.to);ct.innerHTML='<strong>Edge</strong><br><span class=\"url\">'+(fn?fn.label:'?')+'</span> → <span class=\"url\">'+(tn?tn.label:'?')+'</span>';el.style.display='block'}}
  else{el.style.display='none'}
});
network.on('hoverNode',function(){document.body.style.cursor='pointer'});
network.on('blurNode',function(){document.body.style.cursor='default'});
function togglePhysics(){var ph=network.physics.options;network.setOptions({physics:{enabled:!ph.enabled}})}
function closeInfo(){document.getElementById('info').style.display='none'}
function toggleLegend(){var el=document.getElementById('legend'),btn=document.getElementById('legendToggle');if(el.style.display==='block'){el.style.display='none';btn.textContent='Legend'}else{el.style.display='block';btn.textContent='Hide'}}
function toggleGroup(group){
  var allActive=Object.keys(activeGroups).every(function(g){return activeGroups[g]});
  if(allActive){Object.keys(activeGroups).forEach(function(g){activeGroups[g]=false});activeGroups[group]=true}
  else if(activeGroups[group]){activeGroups[group]=false;var any=Object.keys(activeGroups).some(function(g){return activeGroups[g]});if(!any){activeGroups[group]=true}}
  else{activeGroups[group]=true}
  var selGroups={};Object.keys(activeGroups).forEach(function(g){if(activeGroups[g])selGroups[g]=true});
  var selCount=Object.keys(selGroups).length;
  if(selCount===0||selCount===Object.keys(activeGroups).length){
    nodes.clear();edges.clear();nodes.add(allNodes);edges.add(allEdges)
  }else{
    var keepIds={};
    allNodes.forEach(function(n){if(selGroups[n.group])keepIds[n.id]=true});
    var fn=allNodes.filter(function(n){return keepIds[n.id]});
    var fe=allEdges.filter(function(e){return keepIds[e.from]&&keepIds[e.to]});
    nodes.clear();edges.clear();nodes.add(fn);edges.add(fe)
  }
  document.querySelectorAll('#legend .lg-item').forEach(function(el){
    var g=el.getAttribute('data-group');
    if(g&&activeGroups[g]){el.style.opacity='1'}else{el.style.opacity='0.35'}
  });
}
setTimeout(function(){document.getElementById('loading').style.display='none'},5000);
</script></body></html>`,
		jsString(rootURL), visLibrary(),
		legendBlock(),
		d.Stats.TotalNodes, d.Stats.TotalEdges,
		string(nj), string(ej), groupDefs))
}

func renderSources(rootURL string, d *graphData) []byte {
	srcKeys := make([]string, 0, len(d.Sources))
	for s := range d.Sources {
		srcKeys = append(srcKeys, s)
	}
	sort.Strings(srcKeys)

	nodeMap := make(map[string]visNode)
	for _, n := range d.Nodes {
		nodeMap[n.ID] = n
	}

	var pageSB strings.Builder
	pageSB.WriteString(fmt.Sprintf(`<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>Sources — %s</title>%s
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif;background:#fafafa;color:#222}
h1{font-size:20px;padding:12px 16px;background:#f5f5f5;border-bottom:1px solid #ddd}
.subgraph{background:#fff;border:1px solid #ddd;border-radius:6px;margin:12px 16px;box-shadow:0 1px 3px rgba(0,0,0,.06);overflow:hidden}
.subgraph h2{padding:10px 14px;font-size:14px;background:#f0f0f0;border-bottom:1px solid #ddd;cursor:pointer;user-select:none}
.subgraph h2:hover{background:#e8e8e8}
.subgraph .netwrap{height:320px}
.subgraph .netwrap .vis-network{width:100%%;height:100%%}
.info{font-size:12px;color:#666;padding:4px 14px 8px}
</style>
<script>
document.addEventListener('DOMContentLoaded',function(){
  document.querySelectorAll('.subgraph h2').forEach(function(h2){
    h2.addEventListener('click',function(){var n=this.nextElementSibling;n.style.display=n.style.display==='none'?'block':'none'});
  });
});
</script>
</head><body>
<h1>WebMap Graph — Sources <span style="font-size:13px;font-weight:400;color:#666">%s</span></h1>
<div style="padding:8px 16px;font-size:13px"><a href="index.html">← Hub</a> &nbsp;·&nbsp; <a href="full.html">Full Graph</a></div>
`, jsString(rootURL), visLibrary(), jsString(rootURL)))

	for _, src := range srcKeys {
		targetIDs := d.Sources[src]
		srcID := sanitizeID(src)

		var subNodes []visNode
		seen := make(map[string]bool)
		if sn, ok := nodeMap[srcID]; ok {
			subNodes = append(subNodes, sn)
			seen[srcID] = true
		}
		for _, tid := range targetIDs {
			if !seen[tid] {
				if tn, ok := nodeMap[tid]; ok {
					subNodes = append(subNodes, tn)
					seen[tid] = true
				}
			}
		}

		var subEdges []visEdge
		for _, e := range d.Edges {
			if e.From == srcID && seen[e.To] {
				subEdges = append(subEdges, e)
			}
		}

		snj, _ := json.Marshal(subNodes)
		sej, _ := json.Marshal(subEdges)
		srcIDJS := sanitizeID(src)

		pageSB.WriteString(fmt.Sprintf(`<div class="subgraph">
<h2 id="%s">%s <span style="font-weight:400;color:#888">(%d nodes, %d edges)</span></h2>
<div class="netwrap" id="net-%s"></div>
<div class="info">Click heading to collapse/expand · drag nodes to rearrange</div>
</div>
<script>
(function(){var container=document.getElementById('net-%s');
var sn=new vis.DataSet(%s),se=new vis.DataSet(%s);
var sg={%s};
new vis.Network(container,{nodes:sn,edges:se},{groups:sg,nodes:{font:{face:'monospace',size:11},margin:6},edges:{smooth:{type:'curvedCW',roundness:0.12},color:{color:'#888'}},physics:{enabled:true,solver:'barnesHut',barnesHut:{gravitationalConstant:-6000,centralGravity:0.15,springLength:200,springConstant:0.02,damping:0.1},stabilization:{iterations:40,updateInterval:10}},interaction:{hover:true,tooltipDelay:80},layout:{improvedLayout:true,randomSeed:42}})
})();
</script>`,
			srcIDJS, src, len(subNodes), len(subEdges),
			srcIDJS, srcIDJS, string(snj), string(sej), groupDefs))
	}

	pageSB.WriteString("</body></html>")
	return []byte(pageSB.String())
}

func collectParams(queries []string) string {
	paramValues := make(map[string]map[string]bool)
	seen := make(map[string]bool)
	for _, q := range queries {
		if seen[q] {
			continue
		}
		seen[q] = true
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
		if len(vals) > 0 {
			vl := make([]string, 0, len(vals))
			for v := range vals {
				vl = append(vl, v)
			}
			sort.Strings(vl)
			if len(vl) <= 5 {
				parts = append(parts, fmt.Sprintf("%s=%s", name, strings.Join(vl, ",")))
			} else {
				parts = append(parts, fmt.Sprintf("%s=%s+...", name, strings.Join(vl[:5], ",")))
			}
		} else {
			parts = append(parts, name)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// --- public API ---

func (g *Graph) Render() []byte {
	d := g.build()
	return renderFull(g.rootURL, d)
}

func (g *Graph) SaveToDirectory(dirPath string) error {
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	d := g.build()

	files := map[string][]byte{
		"index.html":   renderIndex(g.rootURL, d),
		"full.html":    renderFull(g.rootURL, d),
		"sources.html": renderSources(g.rootURL, d),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dirPath, name), data, 0644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	dj, _ := json.MarshalIndent(d, "", "  ")
	if err := os.WriteFile(filepath.Join(dirPath, "data.json"), dj, 0644); err != nil {
		return fmt.Errorf("write data.json: %w", err)
	}

	return nil
}

func (g *Graph) DefaultFilename() string {
	return "graph.wmap"
}
