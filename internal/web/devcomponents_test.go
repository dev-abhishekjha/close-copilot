package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevComponentsOff(t *testing.T) {
	ts := newTestServer(t)
	ck, _ := ts.session(RoleViewer)
	w := ts.do(t, req{method: http.MethodGet, path: "/dev/components", cookies: []*http.Cookie{ck}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

var devHeadings = []string{"Icon", "Button", "StatusChip", "SeverityBadge", "VerifiedBadge", "Card", "StatTile",
	"DataTable", "Tabs", "Stepper", "Timeline", "Toast", "ConfirmModal", "Field", "EmptyState", "Skeleton"}

func TestDevComponentsOn(t *testing.T) {
	ts := newTestServer(t, func(o *Options) { o.DevComponents = true })
	ck, _ := ts.session(RoleViewer)
	w := ts.do(t, req{method: http.MethodGet, path: "/dev/components", cookies: []*http.Cookie{ck}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, h := range devHeadings {
		if !strings.Contains(body, ">"+h+"</h2>") {
			t.Errorf("missing heading %s", h)
		}
	}
	for _, want := range []string{`<script src="/static/js/ui.js" defer></script>`, "<dialog", `id="ui-toasts"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	for _, bad := range []string{"style=", "<style", " onclick=", "javascript:"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q", bad)
		}
	}

	anon := ts.do(t, req{method: http.MethodGet, path: "/dev/components"})
	if anon.Code == http.StatusOK || strings.Contains(anon.Body.String(), "Components") {
		t.Errorf("anonymous got %d", anon.Code)
	}
}

func TestDevComponentsPreview(t *testing.T) {
	dir := os.Getenv("UI_PREVIEW_DIR")
	if dir == "" {
		t.Skip("UI_PREVIEW_DIR not set")
	}
	ts := newTestServer(t, func(o *Options) { o.DevComponents = true })
	ck, _ := ts.session(RoleViewer)
	w := ts.do(t, req{method: http.MethodGet, path: "/dev/components", cookies: []*http.Cookie{ck}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if err := os.WriteFile(filepath.Join(dir, "components.html"), w.Body.Bytes(), 0o644); err != nil { //nolint:gosec // preview output
		t.Fatal(err)
	}
}

func TestUIScript(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(t, req{method: http.MethodGet, path: "/static/js/ui.js"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("content type = %q", ct)
	}
	src := w.Body.String()
	if !strings.Contains(src, `"use strict"`) {
		t.Error(`missing "use strict"`)
	}
	for _, bad := range []string{"eval(", "new Function", "innerHTML", "document.write", "outerHTML"} {
		if strings.Contains(src, bad) {
			t.Errorf("ui.js contains %q", bad)
		}
	}
}
