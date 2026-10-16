package web

import (
	"encoding/xml"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func readStatic(t testing.TB, name string) string {
	t.Helper()
	b, err := staticFiles.ReadFile("static/" + name)
	if err != nil {
		t.Fatalf("read static/%s: %v", name, err)
	}
	return string(b)
}

var (
	reTokenDecl = regexp.MustCompile(`(--[a-z0-9-]+)\s*:\s*([^;]+);`)
	reHex6      = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// blockBody returns the text between the braces of the first block whose
// selector text contains sel, balancing nested braces.
func blockBody(t testing.TB, css, sel string) string {
	t.Helper()
	i := strings.Index(css, sel)
	if i < 0 {
		t.Fatalf("selector %q not found", sel)
	}
	j := strings.Index(css[i:], "{")
	if j < 0 {
		t.Fatalf("no block after %q", sel)
	}
	start := i + j + 1
	depth := 1
	for k := start; k < len(css); k++ {
		switch css[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[start:k]
			}
		}
	}
	t.Fatalf("unbalanced block after %q", sel)
	return ""
}

func declarations(body string) map[string]string {
	m := map[string]string{}
	for _, d := range reTokenDecl.FindAllStringSubmatch(body, -1) {
		m[d[1]] = strings.TrimSpace(d[2])
	}
	return m
}

func lin(c float64) float64 {
	c /= 255
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func luminance(t testing.TB, hex string) float64 {
	t.Helper()
	if !reHex6.MatchString(hex) {
		t.Fatalf("colour %q is not 6-digit hex", hex)
	}
	var ch [3]float64
	for i := range ch {
		v, err := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		ch[i] = lin(float64(v))
	}
	return 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2]
}

func contrast(t testing.TB, a, b string) float64 {
	la, lb := luminance(t, a), luminance(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

var tokenSemantic = []string{"sev-high", "sev-med", "sev-low", "ok", "warn", "danger", "info"}

var colourTokens = func() []string {
	n := []string{"bg", "surface", "surface-2", "border", "border-strong", "fg", "fg-muted", "fg-subtle",
		"accent", "accent-hover", "accent-soft", "on-accent", "focus"}
	for _, s := range tokenSemantic {
		n = append(n, s, s+"-soft")
	}
	return n
}()

var otherTokens = []string{
	"space-1", "space-2", "space-3", "space-4", "space-5", "space-6", "space-8", "space-10", "space-12",
	"radius-sm", "radius-md", "radius-lg", "radius-pill", "shadow-sm", "shadow-md", "shadow-lg",
	"font-sans", "font-mono", "text-xs", "text-sm", "text-base", "text-md", "text-lg", "text-xl", "text-2xl",
	"leading-tight", "leading-normal", "layout-sidebar-w", "layout-content-max",
	"z-sidebar", "z-topbar", "z-modal", "z-toast", "focus-ring",
}

func TestDesignTokensContrast(t *testing.T) {
	css := readStatic(t, "css/tokens.css")
	light := declarations(blockBody(t, css, ":root {"))
	dark := declarations(blockBody(t, blockBody(t, css, "@media (prefers-color-scheme: dark)"), ":root:not([data-theme=\"light\"])"))
	dark2 := declarations(blockBody(t, css, ":root[data-theme=\"dark\"]"))

	if len(dark) == 0 || len(dark) != len(dark2) {
		t.Fatalf("dark blocks differ in size: %d vs %d", len(dark), len(dark2))
	}
	for k, v := range dark {
		if dark2[k] != v {
			t.Errorf("dark blocks differ at %s: %q vs %q", k, v, dark2[k])
		}
	}

	for _, n := range colourTokens {
		for name, set := range map[string]map[string]string{"light": light, "dark": dark} {
			v, ok := set["--color-"+n]
			if !ok {
				t.Errorf("%s: missing --color-%s", name, n)
				continue
			}
			if !reHex6.MatchString(v) {
				t.Errorf("%s: --color-%s = %q, want 6-digit hex", name, n, v)
			}
		}
	}
	for _, n := range otherTokens {
		if _, ok := light["--"+n]; !ok {
			t.Errorf("light: missing --%s", n)
		}
	}
	for _, n := range []string{"shadow-sm", "shadow-md", "shadow-lg"} {
		if _, ok := dark["--"+n]; !ok {
			t.Errorf("dark: missing --%s", n)
		}
	}

	type pair struct {
		fg, bg string
		min    float64
	}
	var pairs []pair
	for _, f := range []string{"fg", "fg-muted", "fg-subtle"} {
		for _, b := range []string{"bg", "surface", "surface-2"} {
			pairs = append(pairs, pair{f, b, 4.5})
		}
	}
	pairs = append(pairs, pair{"accent", "surface", 4.5}, pair{"on-accent", "accent", 4.5},
		pair{"on-accent", "accent-hover", 4.5}, pair{"focus", "surface", 3}, pair{"focus", "bg", 3})
	for _, s := range tokenSemantic {
		pairs = append(pairs, pair{s, s + "-soft", 4.5})
	}
	for name, set := range map[string]map[string]string{"light": light, "dark": dark} {
		for _, p := range pairs {
			f, b := set["--color-"+p.fg], set["--color-"+p.bg]
			if f == "" || b == "" {
				continue // already reported as missing
			}
			if got := contrast(t, f, b); got < p.min {
				t.Errorf("%s: %s (%s) on %s (%s) contrast %.2f < %.1f", name, p.fg, f, p.bg, b, got, p.min)
			}
		}
	}
}

var (
	reStyleAttr = regexp.MustCompile(`(?i)\bstyle\s*=`)
	reURL       = regexp.MustCompile(`url\(\s*["']?([^"')]*)`)
	reImport    = regexp.MustCompile(`@import\s+(?:url\(\s*)?["']?([^"')\s;]+)`)
)

func TestDesignStylesheetRules(t *testing.T) {
	files := []string{"app.css"}
	err := fs.WalkDir(staticFiles, "static/css", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".css") {
			files = append(files, strings.TrimPrefix(p, "static/"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, f := range files {
		css := readStatic(t, f)
		all.WriteString(css)
		if reStyleAttr.MatchString(css) {
			t.Errorf("%s: contains style=", f)
		}
		for _, m := range reURL.FindAllStringSubmatch(css, -1) {
			u := strings.TrimSpace(m[1])
			if strings.HasPrefix(u, "http:") || strings.HasPrefix(u, "https:") || strings.HasPrefix(u, "//") {
				t.Errorf("%s: external url(%s)", f, u)
			}
			if strings.HasPrefix(u, "/static/") {
				if _, err := staticFiles.ReadFile("static/" + strings.TrimPrefix(u, "/static/")); err != nil {
					t.Errorf("%s: url(%s) does not resolve: %v", f, u, err)
				}
			}
		}
		for _, m := range reImport.FindAllStringSubmatch(css, -1) {
			if !strings.HasPrefix(m[1], "/static/css/") {
				t.Errorf("%s: @import %s outside /static/css/", f, m[1])
				continue
			}
			if _, err := staticFiles.ReadFile("static/" + strings.TrimPrefix(m[1], "/static/")); err != nil {
				t.Errorf("%s: @import %s does not resolve: %v", f, m[1], err)
			}
		}
	}
	for _, want := range []string{"prefers-reduced-motion", ":focus-visible"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("stylesheets lack %s", want)
		}
	}
}

// unstyledOK lists class names the templates use as pure test or script
// hooks, or whose element is styled by its parent's rule.
var unstyledOK = map[string]string{
	"actor": "audit table cell, plain text", "amount": "plain cell; alignment comes from table rules",
	"args": "audit cell, plain text", "audit-row": "row hook for tests", "balanced": "plain cell",
	"cache": "metrics cell", "calls": "metrics cell", "cost": "metrics cell", "count": "hook",
	"counts": "hook", "decision": "styled through table.approvals .decision",
	"detail": "plain cell", "duration": "plain text", "explanation": "plain paragraph",
	"ids": "hook", "input": "hook on the input column", "label": "hook", "latency": "metrics cell",
	"older": "hook", "output": "audit cell", "remark": "plain text", "role-name": "plain text",
	"server": "metrics cell", "time": "plain cell", "title": "plain cell", "tokens": "metrics text",
	"tool": "metrics cell", "type": "plain cell", "verified-rate": "metrics text",
	"audit": "styled as table.audit", "approvals": "styled as table.approvals", "evidence": "styled as table.evidence",
	"lines": "styled as table.approvals table.lines", "steps": "styled as table.steps",
}

func TestDesignLegacyClassesStyled(t *testing.T) {
	reClass := regexp.MustCompile(`class="([^"]+)"`)
	files, err := filepath.Glob("templates/*.templ")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob templates: %v (%d files)", err, len(files))
	}
	classes := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range reClass.FindAllStringSubmatch(string(b), -1) {
			for _, c := range strings.Fields(m[1]) {
				classes[c] = true
			}
		}
	}
	cssFiles, err := fs.Glob(staticFiles, "static/css/*.css")
	if err != nil || len(cssFiles) == 0 {
		t.Fatalf("glob stylesheets: %v (%d files)", err, len(cssFiles))
	}
	var cssAll strings.Builder
	for _, f := range cssFiles {
		cssAll.WriteString(readStatic(t, strings.TrimPrefix(f, "static/")))
	}
	css := cssAll.String()
	var missing []string
	for c := range classes {
		if strings.Contains(css, "."+c) {
			continue
		}
		if _, ok := unstyledOK[c]; ok {
			continue
		}
		missing = append(missing, c)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("classes used by templates but styled nowhere: %v", missing)
	}
}

func TestStaticFonts(t *testing.T) {
	ts := newTestServer(t)
	for _, f := range []string{"inter-latin.woff2", "inter-latin-ext.woff2"} {
		w := ts.do(t, req{method: http.MethodGet, path: "/static/fonts/" + f})
		if w.Code != http.StatusOK {
			t.Errorf("%s = %d", f, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); ct != "font/woff2" {
			t.Errorf("%s content type = %q, want font/woff2", f, ct)
		}
		if !strings.HasPrefix(w.Body.String(), "wOF2") {
			t.Errorf("%s does not start with wOF2 magic", f)
		}
	}
	css := readStatic(t, "css/tokens.css")
	srcs := regexp.MustCompile(`@font-face\s*\{[^}]*src:\s*url\("([^"]+)"\)`).FindAllStringSubmatch(css, -1)
	if len(srcs) != 2 {
		t.Fatalf("@font-face rules = %d, want 2", len(srcs))
	}
	for _, m := range srcs {
		if !strings.HasPrefix(m[1], "/static/fonts/") {
			t.Errorf("font src %s not under /static/fonts/", m[1])
			continue
		}
		if _, err := staticFiles.ReadFile("static/" + strings.TrimPrefix(m[1], "/static/")); err != nil {
			t.Errorf("font src %s does not resolve: %v", m[1], err)
		}
	}
	w := ts.do(t, req{method: http.MethodGet, path: "/static/app.css"})
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
		t.Errorf("app.css content type = %q", w.Header().Get("Content-Type"))
	}
}

func TestIconSprite(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, req{method: http.MethodGet, path: "/static/icons.svg"})
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "image/svg+xml") {
		t.Fatalf("icons.svg = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if strings.Contains(strings.ToLower(body), "<script") {
		t.Error("sprite contains <script>")
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(body) {
		t.Error("sprite contains an on* attribute")
	}
	if regexp.MustCompile(`(?i)(https?:)?//[a-z]`).FindAllString(strings.ReplaceAll(body, "http://www.w3.org/2000/svg", ""), -1) != nil {
		t.Error("sprite contains an external reference")
	}

	dec := xml.NewDecoder(strings.NewReader(body))
	have := map[string]string{}
	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("sprite is not valid XML: %v", err)
			}
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "symbol" {
			var id, vb string
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "id":
					id = a.Value
				case "viewBox":
					vb = a.Value
				}
			}
			have[id] = vb
		}
	}
	ids := []string{"home", "play", "alert", "message", "check-circle", "file-text", "book", "shield", "bar-chart",
		"server", "search", "plus", "check", "x", "chevron-down", "chevron-right", "external", "download", "refresh",
		"user", "log-out", "sun", "moon", "info", "lock", "filter", "copy", "menu", "clock", "trash"}
	for _, id := range ids {
		vb, ok := have["icon-"+id]
		if !ok {
			t.Errorf("missing symbol icon-%s", id)
		} else if vb != "0 0 24 24" {
			t.Errorf("icon-%s viewBox = %q", id, vb)
		}
	}
}
