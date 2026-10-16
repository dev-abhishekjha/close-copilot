package templates

import (
	"context"
	"sync"
)

// The app shell (UI-03): the signed-in page frame drawn by Base when the
// request context carries a ShellData. Which links the frame shows is decided
// by the server from its route table; the frame only draws them.

// NavItem is one sidebar link.
type NavItem struct {
	Label, Href, Icon string
	Current           bool
}

// NavGroup is a titled group of sidebar links. The server leaves out groups
// with no visible item.
type NavGroup struct {
	Title string
	Items []NavItem
}

// Crumb is one breadcrumb. Href is empty for the last crumb (the page itself).
type Crumb struct{ Label, Href string }

// ShellData is what the frame shows for a signed-in request.
type ShellData struct {
	User, Role, CSRFToken string
	Nav                   []NavGroup
	Companies             []string // empty when the runs backend is not configured
	Company, Month        string   // the validated context
	Path                  string   // request path, for the context form's "next"
}

type shellKey struct{}

// shellSource builds the ShellData once, on the first ShellFrom, so a
// request that renders only a fragment never pays for it.
type shellSource struct {
	once  sync.Once
	build func() ShellData
	data  ShellData
}

// WithShell returns ctx carrying d for Base.
func WithShell(ctx context.Context, d ShellData) context.Context {
	return WithShellFunc(ctx, func() ShellData { return d })
}

// WithShellFunc returns ctx carrying build, called at most once, the first
// time a component asks for the shell. A nil build means no shell.
func WithShellFunc(ctx context.Context, build func() ShellData) context.Context {
	return context.WithValue(ctx, shellKey{}, &shellSource{build: build})
}

// ShellFrom returns the ShellData attached to ctx, if any.
func ShellFrom(ctx context.Context) (ShellData, bool) {
	src, ok := ctx.Value(shellKey{}).(*shellSource)
	if !ok || src == nil || src.build == nil {
		return ShellData{}, false
	}
	src.once.Do(func() { src.data = src.build() })
	return src.data, true
}
