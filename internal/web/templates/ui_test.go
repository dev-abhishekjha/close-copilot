package templates

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func render(t testing.TB, c templ.Component) string {
	t.Helper()
	var b bytes.Buffer
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func wantAll(t *testing.T, name, html string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(html, w) {
			t.Errorf("%s: missing %q in:\n%s", name, w, html)
		}
	}
}

func wantNone(t *testing.T, name, html string, bads ...string) {
	t.Helper()
	for _, b := range bads {
		if strings.Contains(html, b) {
			t.Errorf("%s: unexpected %q in:\n%s", name, b, html)
		}
	}
}

func TestUIComponentButton(t *testing.T) {
	tests := []struct {
		name string
		p    ButtonProps
		want []string
		none []string
	}{
		{"zero", ButtonProps{Label: "Go"}, []string{"<button", `type="button"`, "ui-btn-primary", "ui-btn-md", ">Go<"}, []string{"disabled", "aria-busy"}},
		{"secondary small", ButtonProps{Label: "x", Variant: "secondary", Size: "sm"}, []string{"ui-btn-secondary", "ui-btn-sm"}, nil},
		{"danger", ButtonProps{Label: "x", Variant: "danger"}, []string{"ui-btn-danger"}, nil},
		{"ghost", ButtonProps{Label: "x", Variant: "ghost"}, []string{"ui-btn-ghost"}, nil},
		{"unknown variant", ButtonProps{Label: "x", Variant: "zzz"}, []string{"ui-btn-primary"}, []string{"zzz"}},
		{"submit", ButtonProps{Label: "x", Type: "submit"}, []string{`type="submit"`}, nil},
		{"bad type", ButtonProps{Label: "x", Type: "evil"}, []string{`type="button"`}, []string{"evil"}},
		{"href", ButtonProps{Label: "Open", Href: "/runs"}, []string{"<a ", `href="/runs"`}, []string{"<button"}},
		{"loading", ButtonProps{Label: "Wait", Loading: true}, []string{`aria-busy="true"`, "disabled", "ui-spinner"}, nil},
		{"disabled", ButtonProps{Label: "x", Disabled: true}, []string{"disabled"}, nil},
		{"icon only", ButtonProps{Label: "Add item", Icon: "plus", IconOnly: true}, []string{`aria-label="Add item"`, "icon-plus"}, []string{"ui-btn-label"}},
		{"attrs", ButtonProps{Label: "x", Attrs: map[string]any{"id": "b1", "data-x": "y"}}, []string{`id="b1"`, `data-x="y"`}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := render(t, Button(tc.p))
			wantAll(t, tc.name, h, tc.want...)
			wantNone(t, tc.name, h, tc.none...)
		})
	}
	h := render(t, OpenDialogButton(ButtonProps{Label: "Open"}, "dlg"))
	wantAll(t, "open dialog", h, `data-dialog-open="dlg"`, `aria-haspopup="dialog"`)
}

func TestUIComponentBadges(t *testing.T) {
	for _, s := range []string{"queued", "running", "done", "failed", "open", "accepted", "dismissed", "proposed", "approved", "rejected", "posted", "partial"} {
		h := render(t, StatusChip(s))
		wantAll(t, s, h, "ui-chip", strings.ToUpper(s[:1])+s[1:])
	}
	h := render(t, StatusChip("weird"))
	wantAll(t, "unknown", h, "ui-chip-neutral", ">weird<")
	wantNone(t, "unknown", h, "<svg")
	for sev, want := range map[string][]string{
		"high": {"ui-sev-high", "High", "<svg"}, "medium": {"ui-sev-med", "Medium"}, "low": {"ui-sev-low", "Low"}, "odd": {"ui-sev-neutral", "odd"},
	} {
		wantAll(t, sev, render(t, SeverityBadge(sev)), want...)
	}
	wantAll(t, "verified", render(t, VerifiedBadge(true)), "Verified", "ui-verified", "<svg")
	h = render(t, VerifiedBadge(false))
	wantAll(t, "unverified", h, "Unverified", "ui-unverified")
}

func TestUIComponentCardStatTile(t *testing.T) {
	body := templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := w.Write([]byte("<p>inside</p>"))
		return err
	})
	ctx := templ.WithChildren(context.Background(), body)
	var b bytes.Buffer
	if err := Card(CardProps{ID: "c1", Title: "Title", Subtitle: "Sub", Tone: "danger"}).Render(ctx, &b); err != nil {
		t.Fatal(err)
	}
	wantAll(t, "card", b.String(), `<section`, `id="c1"`, `aria-labelledby="c1-title"`, `<h2`, "Title", "Sub", "ui-tone-danger", "inside")
	h := render(t, Card(CardProps{}))
	wantAll(t, "zero card", h, "ui-tone-default")
	wantNone(t, "zero card", h, "<h2", "id=")

	h = render(t, StatTile(StatProps{Label: "Open", Value: "12", Hint: "3 high", Tone: "warn"}))
	wantAll(t, "tile", h, "<div", "Open", "12", "3 high", "ui-tone-warn")
	wantNone(t, "tile", h, "<a ")
	h = render(t, StatTile(StatProps{Label: "Runs", Value: "4", Href: "/runs"}))
	wantAll(t, "tile link", h, "<a ", `href="/runs"`, "ui-stat-link")
}

func TestUIComponentDataTable(t *testing.T) {
	h := render(t, DataTable(TableProps{
		Caption: "Findings",
		Columns: []Column{{Label: "Title", SortHref: "?s=title", Sorted: "asc"}, {Label: "Amount", Align: "right", Sorted: "desc"}, {Label: "Misc"}},
		Rows: []Row{{Cells: []Cell{
			{Text: "A", Href: "/f/1"}, {Text: "1,00", Num: true}, {Content: Icon("check", "ok")},
		}, Attrs: map[string]any{"data-id": "r1", "onclick": "evil()"}}},
		Dense: true,
	}))
	wantAll(t, "table", h, "<caption", "Findings", `scope="col"`, `aria-sort="ascending"`, `aria-sort="descending"`,
		`href="?s=title"`, "ui-align-right", "ui-num", `href="/f/1"`, `aria-label="ok"`, `data-id="r1"`, `tabindex="0"`, `data-dense="true"`)
	wantNone(t, "table", h, "onclick", "Nothing to show")

	h = render(t, DataTable(TableProps{Columns: []Column{{Label: "A"}, {Label: "B"}}, EmptyBody: "No data"}))
	wantAll(t, "empty table", h, "ui-empty", "Nothing to show", "No data", `colspan="2"`, `aria-label="Table"`)
	h = render(t, DataTable(TableProps{EmptyTitle: "None"}))
	wantAll(t, "zero table", h, "None", `colspan="1"`)
}

func TestUIComponentTabsStepperTimeline(t *testing.T) {
	h := render(t, Tabs(TabsProps{Label: "Sections", Items: []Tab{
		{Label: "One", Href: "/1", Current: true}, {Label: "Two", Href: "/2", Count: 7},
	}}))
	wantAll(t, "tabs", h, "<nav", `aria-label="Sections"`, `aria-current="page"`, "ui-tab-count", ">7<")
	if strings.Count(h, "aria-current") != 1 {
		t.Errorf("tabs: aria-current count = %d", strings.Count(h, "aria-current"))
	}
	if strings.Index(h, "One") > strings.Index(h, "Two") {
		t.Error("tabs out of order")
	}

	h = render(t, Stepper([]Step{{Label: "A", State: "done"}, {Label: "B", State: "active", Detail: "now"}, {Label: "C"}, {Label: "D", State: "failed"}}))
	wantAll(t, "stepper", h, "<ol", `aria-current="step"`, "ui-step-done", "ui-step-active", "ui-step-pending", "ui-step-failed",
		"Done", "In progress", "Pending", "Failed", ">now<")
	if strings.Count(h, `aria-current="step"`) != 1 {
		t.Error("stepper: want exactly one active step")
	}
	if strings.Index(h, ">A<") >= strings.Index(h, ">B<") || strings.Index(h, ">B<") >= strings.Index(h, ">C<") {
		t.Error("stepper out of order")
	}

	h = render(t, Timeline([]TimelineEntry{
		{Title: "First", Actor: "maker", Time: "09:00", Detail: "d1", State: "done"}, {Title: "Second"},
	}))
	wantAll(t, "timeline", h, "<ol", "<time", "09:00", "maker", "d1", "First", "Second")
	if strings.Index(h, "First") > strings.Index(h, "Second") {
		t.Error("timeline out of order")
	}
}

func TestUIComponentToastModal(t *testing.T) {
	wantAll(t, "region", render(t, ToastRegion()), `id="ui-toasts"`, `role="status"`, `aria-live="polite"`)
	h := render(t, Toast("error", "Broke"))
	wantAll(t, "toast", h, "ui-toast-error", `data-kind="error"`, "Broke", "data-toast-dismiss", `aria-label="Dismiss"`)
	wantAll(t, "bad kind", render(t, Toast("zzz", "m")), "ui-toast-info")
	wantAll(t, "oob", render(t, ToastOOB("success", "Ok")), `hx-swap-oob="beforeend:#ui-toasts"`, "ui-toast-success")

	h = render(t, ConfirmModal(ConfirmProps{
		ID: "dlg", Title: "Sure?", Body: "Post 10.00", Action: "/x", CSRFToken: "tok",
		Fields: map[string]string{"b": "2", "a": "1"},
	}))
	wantAll(t, "modal", h, "<dialog", `id="dlg"`, `aria-labelledby="dlg-title"`, `method="post"`, `action="/x"`,
		`name="csrf_token" value="tok"`, "data-dialog-close", "Cancel", "Confirm", "ui-btn-danger", `type="submit"`, "Post 10.00")
	if strings.Index(h, `name="a"`) > strings.Index(h, `name="b"`) || !strings.Contains(h, `name="a"`) {
		t.Errorf("hidden fields not sorted:\n%s", h)
	}
	h = render(t, ConfirmModal(ConfirmProps{ID: "d2", Confirm: ButtonProps{Label: "Approve", Variant: "primary"}}))
	wantAll(t, "primary confirm", h, "Approve", "ui-btn-primary")
}

func TestUIComponentFields(t *testing.T) {
	h := render(t, Field(FieldProps{ID: "f", Name: "n", Label: "Name", Hint: "h", Error: "bad", Required: true, Value: "v", Placeholder: "p"}))
	wantAll(t, "field", h, `for="f"`, `id="f"`, `name="n"`, `aria-describedby="f-hint f-error"`, `aria-invalid="true"`,
		`role="alert"`, "required", `value="v"`, `placeholder="p"`, `type="text"`, "bad", ">h<")
	h = render(t, Field(FieldProps{Name: "n", Label: "Name"}))
	wantAll(t, "bare field", h, `id="n"`, `for="n"`)
	wantNone(t, "bare field", h, "aria-describedby", "aria-invalid", "required")
	wantAll(t, "bad type", render(t, Field(FieldProps{Name: "n", Type: "evil"})), `type="text"`)

	h = render(t, SelectField(FieldProps{ID: "s", Name: "m", Label: "Month", Value: "b", Required: true, Error: "e",
		Options: []Option{{"a", "Alpha"}, {"b", "Beta"}}}))
	wantAll(t, "select", h, "<select", `for="s"`, `<option value="a">Alpha`, `<option value="b" selected>Beta`, "required", `aria-invalid="true"`, `aria-describedby="s-error"`)
	h = render(t, TextareaField(FieldProps{ID: "t", Name: "t", Label: "Note", Value: "body", Hint: "h"}))
	wantAll(t, "textarea", h, "<textarea", ">body</textarea>", `aria-describedby="t-hint"`)
}

func TestUIComponentEmptySkeleton(t *testing.T) {
	h := render(t, EmptyState(EmptyProps{Icon: "search", Title: "None", Body: "Try again", Action: &ButtonProps{Label: "Retry", Href: "/r"}}))
	wantAll(t, "empty", h, "None", "Try again", "Retry", `href="/r"`, "<svg")
	wantNone(t, "empty zero", render(t, EmptyState(EmptyProps{})), "<button", "<a ", "<svg")

	h = render(t, Skeleton(2))
	wantAll(t, "skeleton", h, `aria-hidden="true"`, "visually-hidden", "Loading", "ui-skeleton-line")
	if n := strings.Count(h, `class="ui-skeleton-line"`); n != 2 {
		t.Errorf("skeleton lines = %d, want 2", n)
	}
	if n := strings.Count(render(t, Skeleton(0)), "ui-skeleton-line"); n != 3 {
		t.Errorf("default skeleton lines = %d, want 3", n)
	}
	h = render(t, SkeletonTable(2, 3))
	wantAll(t, "skeleton table", h, `aria-hidden="true"`, "Loading")
	if n := strings.Count(h, "<td>"); n != 6 {
		t.Errorf("skeleton cells = %d, want 6", n)
	}
}

func TestUIEscaping(t *testing.T) {
	const x1 = `<script>alert(1)</script>`
	const x2 = `"><img src=x onerror=alert(1)>`
	const js = "javascript:alert(1)"
	attrs := templ.Attributes{"onclick": "alert(1)", "data-ok": "1"}
	for _, x := range []string{x1, x2} {
		cases := map[string]templ.Component{
			"Icon":        Icon("home", x),
			"Button":      Button(ButtonProps{Label: x, Variant: x, Size: x, Type: x, Icon: x, Attrs: attrs}),
			"ButtonHref":  Button(ButtonProps{Label: x, Href: x, IconOnly: true}),
			"StatusChip":  StatusChip(x),
			"Severity":    SeverityBadge(x),
			"Card":        Card(CardProps{ID: x, Title: x, Subtitle: x, Tone: x}),
			"StatTile":    StatTile(StatProps{Label: x, Value: x, Hint: x, Href: x, Tone: x}),
			"DataTable":   DataTable(TableProps{Caption: x, EmptyTitle: x, EmptyBody: x, Columns: []Column{{Label: x, Align: x, SortHref: x, Sorted: x}}}),
			"TableRows":   DataTable(TableProps{Columns: []Column{{Label: x}}, Rows: []Row{{Cells: []Cell{{Text: x, Href: x}}, Attrs: attrs}}}),
			"Tabs":        Tabs(TabsProps{Label: x, Items: []Tab{{Label: x, Href: x}}}),
			"Stepper":     Stepper([]Step{{Label: x, Detail: x, State: x}}),
			"Timeline":    Timeline([]TimelineEntry{{Title: x, Actor: x, Time: x, Detail: x, State: x}}),
			"Toast":       Toast(x, x),
			"ToastOOB":    ToastOOB(x, x),
			"ConfirmForm": ConfirmModal(ConfirmProps{ID: x, Title: x, Body: x, Method: x, Action: x, CSRFToken: x, Confirm: ButtonProps{Label: x}, Fields: map[string]string{x: x}}),
			"Field":       Field(FieldProps{ID: x, Name: x, Label: x, Type: x, Value: x, Hint: x, Error: x, Placeholder: x, Attrs: attrs}),
			"Select":      SelectField(FieldProps{ID: x, Name: x, Label: x, Value: x, Hint: x, Error: x, Options: []Option{{x, x}}, Attrs: attrs}),
			"Textarea":    TextareaField(FieldProps{ID: x, Name: x, Label: x, Value: x, Hint: x, Error: x, Placeholder: x, Attrs: attrs}),
			"Empty":       EmptyState(EmptyProps{Icon: x, Title: x, Body: x, Action: &ButtonProps{Label: x, Href: x}}),
			"StepKey":     Stepper([]Step{{Label: "a", Key: x}}),
			"CellClass":   DataTable(TableProps{Columns: []Column{{Label: "a"}}, Rows: []Row{{Cells: []Cell{{Text: "a", Class: x}}}}}),
			"ConfirmX":    ConfirmModal(ConfirmProps{ID: "d", Extra: Facts([]Fact{{Label: x, Value: x}})}),
			"PageHeader":  PageHeader(PageHeaderProps{Title: x, Subtitle: x}),
			"Facts":       Facts([]Fact{{Label: x, Value: x}}),
		}
		for name, c := range cases {
			h := render(t, c)
			wantNone(t, name, h, "<script", "<img", `" onerror`, "onclick", "<svg onload")
		}
	}
	for name, c := range map[string]templ.Component{
		"Button":  Button(ButtonProps{Label: "x", Href: js}),
		"Tile":    StatTile(StatProps{Label: "x", Href: js}),
		"Tabs":    Tabs(TabsProps{Items: []Tab{{Label: "x", Href: js}}}),
		"Cell":    DataTable(TableProps{Columns: []Column{{Label: "x"}}, Rows: []Row{{Cells: []Cell{{Text: "x", Href: js}}}}}),
		"Sort":    DataTable(TableProps{Columns: []Column{{Label: "x", SortHref: js}}}),
		"Confirm": ConfirmModal(ConfirmProps{ID: "d", Action: js}),
	} {
		wantNone(t, name, render(t, c), `href="javascript:`, `action="javascript:`)
	}
	h := render(t, Button(ButtonProps{Label: "x", Attrs: attrs}))
	wantAll(t, "attrs kept", h, `data-ok="1"`)
	wantNone(t, "attrs dropped", h, "onclick")
	h = render(t, Button(ButtonProps{Label: "x", Attrs: templ.Attributes{"class": "evil", "style": "x", "hx-on:click": "y", "ONCLICK": "z"}}))
	wantNone(t, "attrs reserved", h, "evil", "style=", "hx-on", "ONCLICK", "z")
}

func TestUIHooksStepKey(t *testing.T) {
	h := render(t, Stepper([]Step{{Label: "a", Key: "checks"}, {Label: "b"}}))
	wantAll(t, "key", h, `data-step="checks"`)
	if n := strings.Count(h, "data-step"); n != 1 {
		t.Errorf("data-step count = %d, want 1", n)
	}
	h = render(t, Stepper([]Step{{Label: "a", Key: `"><script>x</script>`}}))
	wantNone(t, "hostile key", h, "<script", `"><script`)
}

func TestUIHooksCellClass(t *testing.T) {
	tests := []struct {
		name, class string
		want        bool
	}{
		{"valid", "amount", true},
		{"multi", "status ok-1", true},
		{"attr break", `onclick" x`, false},
		{"uppercase", "Amount", false},
		{"empty", "", false},
		{"too long", "a" + strings.Repeat("b", 64), false},
		{"leading digit", "1a", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := render(t, DataTable(TableProps{Columns: []Column{{Label: "a"}}, Rows: []Row{{Cells: []Cell{{Text: "v", Class: tc.class}}}}}))
			if tc.want {
				wantAll(t, tc.name, h, `<td class="`+tc.class+`"`)
				return
			}
			wantNone(t, tc.name, h, "onclick", "Amount", "bbbbbbbb")
		})
	}
	h := render(t, DataTable(TableProps{Columns: []Column{{Label: "a", Align: "right"}}, Rows: []Row{{Cells: []Cell{{Text: "v", Num: true, Class: "amount"}}}}}))
	wantAll(t, "merged", h, `class="ui-num ui-align-right amount"`)
}

func TestUIHooksConfirmExtra(t *testing.T) {
	base := ConfirmProps{ID: "d", Title: "T", Body: "BODYTEXT", Action: "/x", CSRFToken: "tok"}
	withExtra := base
	withExtra.Extra = TextareaField(FieldProps{ID: "why", Name: "reason", Label: "EXTRAMARK", Required: true})
	h := render(t, ConfirmModal(withExtra))
	b, e, a := strings.Index(h, "BODYTEXT"), strings.Index(h, "EXTRAMARK"), strings.Index(h, "ui-dialog-actions")
	f := strings.Index(h, "</form>")
	if b < 0 || e < 0 || a < 0 || f < 0 || b >= e || e >= a || a >= f {
		t.Errorf("order wrong: body=%d extra=%d actions=%d form end=%d in:\n%s", b, e, a, f, h)
	}
	wantAll(t, "form parts", h, `name="csrf_token"`, `data-dialog-close`)
	plain := render(t, ConfirmModal(base))
	wantNone(t, "no extra", plain, "EXTRAMARK", "<textarea")
	want := `<p class="ui-dialog-body" id="d-body">BODYTEXT</p><input type="hidden" name="csrf_token"`
	wantAll(t, "unchanged", plain, want)
}

func TestUIHooksPageHeaderFacts(t *testing.T) {
	h := render(t, PageHeader(PageHeaderProps{Title: "Run <1>"}))
	if n := strings.Count(h, "<h1"); n != 1 {
		t.Errorf("h1 count = %d", n)
	}
	wantAll(t, "title escaped", h, "Run &lt;1&gt;", `class="ui-page-header"`)
	wantNone(t, "optional", h, "ui-page-subtitle", "ui-page-actions")
	h = render(t, PageHeader(PageHeaderProps{Title: "T", Subtitle: "Sub", Actions: Button(ButtonProps{Label: "Go"})}))
	wantAll(t, "full", h, "ui-page-subtitle", "Sub", "ui-page-actions", "Go")
	h = render(t, Facts([]Fact{{Label: "A", Value: "v1"}, {Label: "B", Value: "ignored", Content: StatusChip("open")}}))
	wantAll(t, "facts", h, `<dl class="ui-facts">`, "<dt>A</dt>", "v1", "Open")
	wantNone(t, "content replaces value", h, "ignored")
}

func TestUIChipPartial(t *testing.T) {
	h := render(t, StatusChip("partial"))
	wantAll(t, "partial", h, "ui-chip-warn", ">Partial<", "<svg")
}
