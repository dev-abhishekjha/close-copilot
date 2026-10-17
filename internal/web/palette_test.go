package web

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// UI-13: the command palette, shortcuts help, toast region and announcer are
// drawn by the shell; ui.js is checked statically (there is no JS runtime).

func paletteBody(t testing.TB, ts *testServer, role Role, path string) string {
	t.Helper()
	ck, _ := ts.session(role)
	return okBody(t, ts, req{method: http.MethodGet, path: path, cookies: []*http.Cookie{ck}})
}

func paletteList(t testing.TB, body string) string {
	t.Helper()
	return between(t, body, `<ul id="ui-palette-list"`, "</ul>")
}

func TestPaletteRendersRoleLinks(t *testing.T) {
	ts := newTestServer(t)
	for _, role := range []Role{RoleViewer, RoleMaker, RoleChecker} {
		t.Run(string(role), func(t *testing.T) {
			body := paletteBody(t, ts, role, "/")
			side := hrefsIn(mainNav(t, body))
			pal := hrefsIn(paletteList(t, body))
			if !slices.Equal(side, pal) {
				t.Errorf("palette hrefs = %v, sidebar = %v", pal, side)
			}
			for _, e := range navTable {
				if !ts.allows(role, http.MethodGet, e.path) && slices.Contains(pal, e.path) {
					t.Errorf("%s sees %s in the palette", role, e.path)
				}
			}
			if role != RoleChecker && (slices.Contains(pal, "/approvals") || slices.Contains(pal, "/audit")) {
				t.Errorf("%s palette has a checker-only link: %v", role, pal)
			}
		})
	}
}

func TestPaletteMarkup(t *testing.T) {
	ts := newTestServer(t)
	body := paletteBody(t, ts, RoleChecker, "/")
	dlg := between(t, body, `<dialog id="ui-palette"`, "</dialog>")
	for _, want := range []string{
		`role="combobox"`, `aria-controls="ui-palette-list"`, `id="ui-palette-list"`, `role="listbox"`,
		`<p id="ui-palette-empty" class="ui-palette-empty" hidden>`, `aria-label="Command palette"`,
	} {
		if !strings.Contains(dlg, want) {
			t.Errorf("palette lacks %s", want)
		}
	}
	ids := regexp.MustCompile(`role="option"[^>]*id="(ui-palette-opt-\d+)"`).FindAllStringSubmatch(dlg, -1)
	if len(ids) != len(hrefsIn(mainNav(t, body))) {
		t.Errorf("%d options, want one per link", len(ids))
	}
	seen := map[string]bool{}
	for _, m := range ids {
		if seen[m[1]] {
			t.Errorf("duplicate option id %s", m[1])
		}
		seen[m[1]] = true
	}
	btn := between(t, body, `<button type="button" class="shell-palette-btn"`, "</button>")
	for _, want := range []string{`data-dialog-open="ui-palette"`, `aria-keyshortcuts="Control+K Meta+K"`} {
		if !strings.Contains(btn, want) {
			t.Errorf("trigger lacks %s", want)
		}
	}
}

func TestPaletteTrigger(t *testing.T) {
	ts := newTestServer(t)
	body := paletteBody(t, ts, RoleViewer, "/")
	top := between(t, body, `<header class="topbar shell-top">`, "</header>")
	if !strings.Contains(top, `class="shell-palette-btn" data-dialog-open="ui-palette"`) {
		t.Error("trigger is not in the top bar")
	}
	if !strings.Contains(body, `<dialog id="ui-palette"`) {
		t.Error("no dialog for the trigger")
	}
}

func TestPaletteEscapesLabels(t *testing.T) {
	const evil = `<script>alert(1)</script>`
	old := navTable
	t.Cleanup(func() { navTable = old })
	navTable = slices.Clone(old)
	navTable[0].label = evil
	ts := newTestServer(t)
	body := paletteBody(t, ts, RoleViewer, "/")
	if strings.Contains(body, evil) {
		t.Fatal("label not escaped")
	}
	esc := `&lt;script&gt;alert(1)&lt;/script&gt;`
	pal := between(t, body, `<dialog id="ui-palette"`, "</dialog>")
	help := between(t, body, `<dialog id="ui-shortcuts"`, "</dialog>")
	if !strings.Contains(pal, esc) || !strings.Contains(help, esc) {
		t.Error("escaped label missing from the palette or the shortcuts list")
	}
}

func TestPaletteShortcutKeys(t *testing.T) {
	re := regexp.MustCompile(`^g [a-z]$`)
	seen := map[string]bool{}
	for _, e := range navTable {
		if !re.MatchString(e.key) {
			t.Errorf("%s key %q is not g plus a letter", e.label, e.key)
		}
		if seen[e.key] {
			t.Errorf("key %q repeats", e.key)
		}
		seen[e.key] = true
	}
	ts := newTestServer(t)
	for _, role := range []Role{RoleViewer, RoleChecker} {
		list := paletteList(t, paletteBody(t, ts, role, "/"))
		for _, e := range navTable {
			has := strings.Contains(list, `data-shortcut="`+e.key+`"`)
			if want := ts.allows(role, http.MethodGet, e.path); has != want {
				t.Errorf("%s %s: shortcut rendered = %v, want %v", role, e.path, has, want)
			}
		}
	}
}

func TestPaletteShortcutsDialog(t *testing.T) {
	ts := newTestServer(t)
	body := paletteBody(t, ts, RoleChecker, "/")
	dlg := between(t, body, `<dialog id="ui-shortcuts"`, "</dialog>")
	for _, want := range []string{
		`id="ui-shortcuts-title"`, "Keyboard shortcuts", "<kbd>Ctrl</kbd> or <kbd>Cmd</kbd>", "Open the command palette",
		"<kbd>?</kbd>", "Go to a page", "<kbd>Esc</kbd>", "Close a dialog", "Close runs", "Audit log",
	} {
		if !strings.Contains(dlg, want) {
			t.Errorf("shortcuts dialog lacks %s", want)
		}
	}
	if !strings.Contains(body, `data-dialog-open="ui-shortcuts"`) {
		t.Error("no button opens the shortcuts dialog")
	}
}

func TestPaletteToastRegion(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.DevComponents = true })
	for _, path := range []string{"/", "/dev/components"} {
		body := paletteBody(t, ts, RoleViewer, path)
		if n := strings.Count(body, `id="ui-toasts"`); n != 1 {
			t.Errorf("%s: %d toast regions, want 1", path, n)
		}
		if n := strings.Count(body, `id="ui-announce"`); n != 1 {
			t.Errorf("%s: %d announcers, want 1", path, n)
		}
		if !strings.Contains(body, `id="ui-announce" class="visually-hidden" role="status" aria-live="polite"`) {
			t.Errorf("%s: announcer lacks aria-live", path)
		}
	}
	login := ts.do(t, req{method: http.MethodGet, path: "/login"}).Body.String()
	if strings.Contains(login, "ui-toasts") || strings.Contains(login, "ui-announce") {
		t.Error("login page has the toast region or announcer")
	}
}

func TestPaletteScriptTags(t *testing.T) {
	ts := newTestServer(t)
	ck, _ := ts.session(RoleChecker)
	w := ts.do(t, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}})
	body := w.Body.String()
	if n := strings.Count(body, "<script"); n != 2 {
		t.Errorf("%d script tags, want 2", n)
	}
	for _, re := range []string{`style=`, `\son[a-z]+=`, `javascript:`} {
		if regexp.MustCompile(re).MatchString(body) {
			t.Errorf("body matches %s", re)
		}
	}
	if got := w.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("CSP = %q, want %q", got, contentSecurityPolicy)
	}
}

func TestPaletteScriptSafety(t *testing.T) {
	ts := newTestServer(t)
	ck, _ := ts.session(RoleViewer)
	w := ts.do(t, req{method: http.MethodGet, path: "/static/js/ui.js", cookies: []*http.Cookie{ck}})
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Errorf("ui.js = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	js := readStatic(t, "js/ui.js")
	if !strings.HasPrefix(strings.TrimSpace(js), `"use strict";`) {
		t.Error(`ui.js does not start with "use strict"`)
	}
	for _, bad := range []string{"eval(", "new Function", "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", `setTimeout("`, `setTimeout('`} {
		if strings.Contains(js, bad) {
			t.Errorf("ui.js contains %q", bad)
		}
	}
	for _, want := range []string{`"cc:toast"`, `"cc:announce"`, `"htmx:responseError"`, `"htmx:sendError"`, "ui-palette", "data-shortcut"} {
		if !strings.Contains(js, want) {
			t.Errorf("ui.js lacks %s", want)
		}
	}
}

func TestPaletteScriptNoThirdParty(t *testing.T) {
	js := readStatic(t, "js/ui.js")
	for _, bad := range []string{"http://", "https://", "//cdn", "fetch(", "XMLHttpRequest"} {
		if strings.Contains(js, bad) {
			t.Errorf("ui.js contains %q", bad)
		}
	}
}

func TestPaletteCSS(t *testing.T) {
	css := readStatic(t, "css/shell.css")
	for _, c := range []string{
		"shell-palette-btn", "shell-palette-label", "shell-shortcuts-btn", "ui-palette", "ui-palette-head", "ui-palette-input",
		"ui-palette-close", "ui-palette-list", "ui-palette-item", "ui-palette-label", "ui-palette-group", "ui-palette-keys",
		"ui-palette-empty", "ui-shortcuts-body", "ui-shortcuts-list", "ui-shortcuts-row", "ui-shortcuts-close",
	} {
		if !strings.Contains(css, "."+c) {
			t.Errorf("shell.css does not style .%s", c)
		}
	}
	if regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgb\(|hsl\(`).MatchString(css[strings.Index(css, "Command palette, shortcuts help"):]) {
		t.Error("raw colour in the palette styles")
	}
	if strings.Contains(css, "style=") {
		t.Error("style attribute in shell.css")
	}
}

func TestPaletteNavUnchanged(t *testing.T) {
	ts := newTestServer(t)
	body := paletteBody(t, ts, RoleChecker, "/")
	nav := mainNav(t, body)
	want := []string{"/", "/runs", "/findings", "/ask", "/approvals", "/documents", "/audit", "/system"}
	if got := hrefsIn(nav); !slices.Equal(got, want) {
		t.Errorf("sidebar hrefs = %v, want %v", got, want)
	}
	if n := strings.Count(nav, `aria-current="page"`); n != 1 || !strings.Contains(nav, `href="/" aria-current="page"`) {
		t.Errorf("aria-current wrong in %s", nav)
	}
}
