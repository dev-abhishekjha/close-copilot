---
id: UI-03a
title: "Page stylesheet slots and shared component hooks"
phase: 6
owner_role: implementer
risk: standard
approved_by:
depends_on: [UI-03]
needs_erpnext: false
files:
  - internal/web/static/app.css
  - internal/web/static/css/components.css
  - internal/web/static/css/runs.css
  - internal/web/static/css/finding.css
  - internal/web/static/css/ask.css
  - internal/web/design_test.go
  - internal/web/templates/ui.go
  - internal/web/templates/ui_basic.templ
  - internal/web/templates/ui_basic_templ.go
  - internal/web/templates/ui_data.templ
  - internal/web/templates/ui_data_templ.go
  - internal/web/templates/ui_overlay.templ
  - internal/web/templates/ui_overlay_templ.go
  - internal/web/templates/ui_test.go
  - internal/web/templates/dev_components.templ
  - internal/web/templates/dev_components_templ.go
consumes: ["UI-02 components (templates/ui*.templ, ui.go)", "UI-01 stylesheets", "design_test.go stylesheet coverage test"]
produces: ["css/runs.css, css/finding.css, css/ask.css (empty slots imported by app.css)", "templates.PageHeader and templates.Facts", "Step.Key, Cell.Class, ConfirmProps.Extra", "TestDesignLegacyClassesStyled reads every css/*.css"]
acceptance:
  - go test ./internal/web/... -race -count=1
  - go test ./internal/web/templates/ ./internal/web/ -race -count=1 -run 'TestUIComponent|TestUIEscaping|TestUIHooks|TestDesign|TestDevComponents'
  - test "$(go test ./internal/web/templates/ -list 'TestUIHooks' | grep -c '^Test')" -ge 4
  - bash -c 'd=$(mktemp -d) && cp -r internal/web/templates/. "$d" && go tool templ generate && diff -r "$d" internal/web/templates'
  - go vet ./internal/web/...
gates: [G1, G2, G3]
budget: {max_attempts: 3, max_wall_minutes: 90}
---

# UI-03a · Page stylesheet slots and shared component hooks

## Goal

Let the page tickets UI-05 (runs), UI-07 (finding detail) and UI-08 (ask) be built in parallel without editing the same files: each gets its own empty stylesheet already wired in, the design test covers every stylesheet by glob, and the few component hooks all three need exist once, additively.

## Context

- Why a prep ticket: those three tickets would otherwise all edit `app.css`, `design_test.go`, `ui.go` and the component templates, which G0 refuses for parallel work.
- **Additive only.** Existing component output (HTML) must stay byte-identical for every existing caller: the UI-02 tests and all page tests pass unchanged. New fields default to the zero value, which renders exactly what is rendered today.
- CSP and all UI-01/02 rules still apply: no inline style or script, escaped props, no `templ.Raw`, `href` as plain strings.

## What to add

1. **Stylesheet slots.** Create `static/css/runs.css`, `static/css/finding.css`, `static/css/ask.css`, each holding only a header comment (`/* Runs pages (UI-05). */` etc.), and import them in `app.css` after `shell.css` and `components.css`. Page tickets put their page-specific rules there and must not edit `app.css` again.
2. **Design test.** `TestDesignLegacyClassesStyled` (`design_test.go`) reads a hard-coded list of stylesheets. Make it read every file matching `static/css/*.css` (via `staticFiles`), so a new page stylesheet is covered with no test edit. Do not touch `unstyledOK`. `TestDesignStylesheetRules` already walks the css directory: confirm the three new files pass it.
3. **`Step.Key string`** (Stepper): when non-empty the step's `<li>` also carries `data-step="<key>"` (escaped). Needed so run pages keep their `data-step` hooks.
4. **`Cell.Class string`** (DataTable): extra class(es) on the `<td>`, accepted only if it matches `^[a-z][a-z0-9_ -]{0,63}$`, otherwise dropped. Needed so cells keep hooks such as `amount`, `status`.
5. **`ConfirmProps.Extra templ.Component`**: rendered inside the dialog's form after the body text and before the action buttons (for a required textarea such as a dismissal reason). The form, hidden CSRF field and buttons are unchanged.
6. **`PageHeader(p PageHeaderProps)`**: `PageHeaderProps{Title, Subtitle string; Actions templ.Component}`; renders `<header class="ui-page-header">` with the page's one `<h1>`, an optional subtitle `<p>` and an actions slot (right-aligned on wide screens, wrapping below on narrow).
7. **`Facts(items []Fact)`**: `Fact{Label, Value string; Content templ.Component}` renders a `<dl class="ui-facts">` of label/value pairs (`Content` replaces `Value` when set, for badges and links); two columns on wide screens, one on narrow.
8. CSS for 6 and 7 in `components.css`, tokens only, light and dark. Show `PageHeader` (with and without actions) and `Facts` on `/dev/components`; add `Step.Key` and `Cell.Class` to the existing dev examples.
9. Tests `TestUIHooks*` in `templates/ui_test.go`:
   - `TestUIHooksStepKey`: key renders `data-step`, empty key renders none, a hostile key is escaped.
   - `TestUIHooksCellClass`: valid class rendered, `onclick" x`, uppercase, empty and over-long values dropped.
   - `TestUIHooksConfirmExtra`: the extra component renders inside the form between the body and the buttons; without it the output equals the UI-02 output.
   - `TestUIHooksPageHeaderFacts`: one `<h1>`, subtitle and actions optional, escaping of title, facts with `Content`.
   - Extend `TestUIEscaping`'s hostile-string sweep to the new props.

## Code map

- `internal/web/templates/ui.go`: props structs (`Step`, `Cell`, `ConfirmProps`, ...); `ui_data.templ` (`Stepper`, `DataTable`), `ui_overlay.templ` (`ConfirmModal`), `ui_basic.templ` (cards, badges): add the new templates to `ui_basic.templ`.
- `internal/web/design_test.go:256` `TestDesignLegacyClassesStyled`, `readStatic`.
- `internal/web/static/app.css`: the `@import` list.
- `internal/web/templates/dev_components.templ`: the showcase page.
- Run `make generate` after editing templ files; generated files are committed.

## Out of scope

- Using any of this on a real page (UI-05, UI-07, UI-08), a new component beyond `PageHeader` and `Facts`.

## Notes

- No new dependencies.
- If a UI-02 test pins exact HTML of a component this ticket touches and the new output differs for the zero value, that is a bug in this ticket: fix the code, not the test.
