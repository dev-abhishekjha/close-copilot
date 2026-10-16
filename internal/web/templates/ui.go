package templates

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/a-h/templ"
)

// This file holds the props and small helpers of the shared UI components
// (UI-02). The components themselves live in ui_basic.templ, ui_data.templ
// and ui_overlay.templ. Later UI tickets compose them, so the signatures are
// a contract.

// ButtonProps describes Button and OpenDialogButton.
type ButtonProps struct {
	Label    string
	Variant  string // primary (default), secondary, danger, ghost
	Size     string // md (default), sm
	Type     string // button (default), submit, reset
	Icon     string
	Href     string
	Disabled bool
	Loading  bool
	IconOnly bool
	Attrs    templ.Attributes
}

// CardProps describes Card.
type CardProps struct {
	ID       string
	Title    string
	Subtitle string
	Actions  templ.Component
	Tone     string // default, danger, warn, info
}

// StatProps describes StatTile.
type StatProps struct {
	Label, Value, Hint, Href, Tone string
}

// Column is one DataTable header.
type Column struct {
	Label    string
	Align    string // left (default), right, center
	SortHref string
	Sorted   string // asc, desc or empty
}

// Cell is one DataTable cell. Content, when set, replaces Text and Href.
type Cell struct {
	Text    string
	Href    string
	Num     bool
	Content templ.Component
	Class   string // extra class(es) on the <td>; dropped unless it matches cellExtraClass
}

// Row is one DataTable row.
type Row struct {
	Cells []Cell
	Attrs templ.Attributes
}

// TableProps describes DataTable.
type TableProps struct {
	Caption    string
	Columns    []Column
	Rows       []Row
	EmptyTitle string
	EmptyBody  string
	Dense      bool
}

// Tab is one link of Tabs.
type Tab struct {
	Label   string
	Href    string
	Count   int
	Current bool
}

// TabsProps describes Tabs.
type TabsProps struct {
	Label string
	Items []Tab
}

// Step is one Stepper step. State is done, active, pending or failed.
type Step struct {
	Label, Detail, State string
	Key                  string // optional data-step hook
}

// TimelineEntry is one Timeline item. Time is formatted by the caller.
type TimelineEntry struct {
	Title, Actor, Time, Detail, State string
}

// ConfirmProps describes ConfirmModal.
type ConfirmProps struct {
	ID, Title, Body, Method, Action, CSRFToken string
	Confirm                                    ButtonProps
	Fields                                     map[string]string
	Extra                                      templ.Component // rendered after the body, before the buttons
}

// Option is one SelectField option.
type Option struct {
	Value, Label string
}

// FieldProps describes Field, SelectField and TextareaField.
type FieldProps struct {
	ID, Name, Label, Type, Value, Hint, Error, Placeholder string
	Required                                               bool
	Options                                                []Option
	Attrs                                                  templ.Attributes
}

// EmptyProps describes EmptyState.
type EmptyProps struct {
	Icon, Title, Body string
	Action            *ButtonProps
}

var knownIcons = map[string]bool{
	"home": true, "play": true, "alert": true, "message": true, "check-circle": true, "file-text": true,
	"book": true, "shield": true, "bar-chart": true, "server": true, "search": true, "plus": true,
	"check": true, "x": true, "chevron-down": true, "chevron-right": true, "external": true,
	"download": true, "refresh": true, "user": true, "log-out": true, "sun": true, "moon": true,
	"info": true, "lock": true, "filter": true, "copy": true, "menu": true, "clock": true, "trash": true,
}

func iconHref(name string) string { return "/static/icons.svg#icon-" + name }

func oneOf(v, def string, allowed ...string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return def
}

// safeAttrs drops caller attributes that could break the CSP or the
// component's own contract: event handlers, class, style, href and src.
func safeAttrs(in templ.Attributes) templ.Attributes {
	if len(in) == 0 {
		return nil
	}
	out := make(templ.Attributes, len(in))
	for k, v := range in {
		l := strings.ToLower(strings.TrimSpace(k))
		switch {
		case l == "", strings.HasPrefix(l, "on"), strings.HasPrefix(l, "hx-on"),
			l == "class", l == "style", l == "href", l == "src", l == "action", l == "formaction":
			continue
		}
		out[k] = v
	}
	return out
}

func buttonType(p ButtonProps) string { return oneOf(p.Type, "button", "button", "submit", "reset") }

func buttonClass(p ButtonProps) string {
	c := "ui-btn ui-btn-" + oneOf(p.Variant, "primary", "primary", "secondary", "danger", "ghost") +
		" ui-btn-" + oneOf(p.Size, "md", "md", "sm")
	if p.IconOnly {
		c += " ui-btn-icon"
	}
	if p.Loading {
		c += " ui-btn-loading"
	}
	return c
}

func buttonInert(p ButtonProps) bool { return p.Disabled || p.Loading }

// withAttrs returns p with extra attributes merged over its own.
func withAttrs(p ButtonProps, kv map[string]any) ButtonProps {
	m := make(templ.Attributes, len(p.Attrs)+len(kv))
	for k, v := range p.Attrs {
		m[k] = v
	}
	for k, v := range kv {
		m[k] = v
	}
	p.Attrs = m
	return p
}

func dialogButton(p ButtonProps, dialogID string) ButtonProps {
	return withAttrs(p, map[string]any{"data-dialog-open": dialogID, "aria-haspopup": "dialog"})
}

type chip struct{ class, icon, label string }

var statusChips = map[string]chip{
	"queued":    {"neutral", "clock", "Queued"},
	"running":   {"info", "refresh", "Running"},
	"done":      {"ok", "check-circle", "Done"},
	"failed":    {"danger", "alert", "Failed"},
	"open":      {"warn", "alert", "Open"},
	"accepted":  {"ok", "check", "Accepted"},
	"dismissed": {"neutral", "x", "Dismissed"},
	"proposed":  {"info", "file-text", "Proposed"},
	"approved":  {"ok", "check-circle", "Approved"},
	"rejected":  {"danger", "x", "Rejected"},
	"posted":    {"ok", "check", "Posted"},
}

func statusChip(s string) chip {
	if c, ok := statusChips[strings.ToLower(strings.TrimSpace(s))]; ok {
		return c
	}
	return chip{"neutral", "", s}
}

var severityChips = map[string]chip{
	"high":   {"high", "alert", "High"},
	"medium": {"med", "alert", "Medium"},
	"low":    {"low", "info", "Low"},
}

func severityChip(s string) chip {
	if c, ok := severityChips[strings.ToLower(strings.TrimSpace(s))]; ok {
		return c
	}
	return chip{"neutral", "", s}
}

func toneClass(tone string) string {
	return "ui-tone-" + oneOf(tone, "default", "default", "danger", "warn", "info")
}

func cardID(p CardProps) string { return p.ID }

func cardTitleID(p CardProps) string {
	if p.ID == "" {
		return ""
	}
	return p.ID + "-title"
}

func colAlign(cols []Column, i int) string {
	if i < len(cols) {
		return oneOf(cols[i].Align, "", "right", "center")
	}
	return ""
}

func thClass(c Column) string {
	if a := colAlign([]Column{c}, 0); a != "" {
		return "ui-align-" + a
	}
	return ""
}

func cellClass(cols []Column, i int, c Cell) string {
	cls := ""
	if c.Num {
		cls = "ui-num"
	}
	if a := colAlign(cols, i); a != "" {
		cls = strings.TrimSpace(cls + " ui-align-" + a)
	}
	if x := cellExtra(c.Class); x != "" {
		cls = strings.TrimSpace(cls + " " + x)
	}
	return cls
}

var cellExtraClass = regexp.MustCompile(`^[a-z][a-z0-9_ -]{0,63}$`)

func cellExtra(s string) string {
	if cellExtraClass.MatchString(s) {
		return s
	}
	return ""
}

func ariaSort(s string) string {
	switch s {
	case "asc":
		return "ascending"
	case "desc":
		return "descending"
	}
	return ""
}

func colspan(cols []Column) string { return strconv.Itoa(max(1, len(cols))) }

func tableLabel(p TableProps) string {
	if p.Caption != "" {
		return p.Caption
	}
	return "Table"
}

func emptyTitle(p TableProps) string {
	if p.EmptyTitle != "" {
		return p.EmptyTitle
	}
	return "Nothing to show"
}

func tabCount(n int) string { return strconv.Itoa(n) }

var stepStates = map[string]string{"done": "Done", "active": "In progress", "pending": "Pending", "failed": "Failed"}

func stepState(s string) string {
	if _, ok := stepStates[s]; ok {
		return s
	}
	return "pending"
}

func stepText(s string) string { return stepStates[stepState(s)] }

func timelineState(s string) string {
	if _, ok := stepStates[s]; ok {
		return s
	}
	return "done"
}

func toastKind(k string) string { return oneOf(k, "info", "success", "error", "info") }

func toastIcon(k string) string {
	switch toastKind(k) {
	case "success":
		return "check-circle"
	case "error":
		return "alert"
	}
	return "info"
}

func formMethod(m string) string { return oneOf(strings.ToLower(m), "post", "post", "get") }

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func confirmButton(p ConfirmProps) ButtonProps {
	b := p.Confirm
	if b.Variant == "" {
		b.Variant = "danger"
	}
	if b.Label == "" {
		b.Label = "Confirm"
	}
	b.Type = "submit"
	b.Href = ""
	return b
}

func fieldID(p FieldProps) string {
	if p.ID != "" {
		return p.ID
	}
	return p.Name
}

func hintID(p FieldProps) string  { return fieldID(p) + "-hint" }
func errorID(p FieldProps) string { return fieldID(p) + "-error" }

func describedBy(p FieldProps) string {
	var ids []string
	if p.Hint != "" {
		ids = append(ids, hintID(p))
	}
	if p.Error != "" {
		ids = append(ids, errorID(p))
	}
	return strings.Join(ids, " ")
}

func inputType(t string) string {
	return oneOf(t, "text", "text", "email", "number", "date", "password", "search", "tel", "url", "month")
}

func count(n, def, limit int) int {
	if n <= 0 {
		return def
	}
	return min(n, limit)
}

// PageHeaderProps describes PageHeader.
type PageHeaderProps struct {
	Title, Subtitle string
	Actions         templ.Component
}

// Fact is one label/value pair of Facts. Content replaces Value when set.
type Fact struct {
	Label, Value string
	Content      templ.Component
}
