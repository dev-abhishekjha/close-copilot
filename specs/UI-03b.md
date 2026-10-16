---
id: UI-03b
title: "Route hooks, page stylesheet slots and the partial chip"
phase: 6
owner_role: implementer
risk: standard
approved_by:
depends_on: [UI-03a, UI-08]
needs_erpnext: false
files:
  - internal/web/routes.go
  - internal/web/home.go
  - internal/web/routes_dashboard.go
  - internal/web/routes_findings.go
  - internal/web/routes_proposals.go
  - internal/web/routes_documents.go
  - internal/web/routes_system.go
  - internal/web/routes_audit.go
  - internal/web/routes_hooks_test.go
  - internal/web/static/app.css
  - internal/web/static/css/dashboard.css
  - internal/web/static/css/findings-list.css
  - internal/web/static/css/proposals.css
  - internal/web/static/css/audit.css
  - internal/web/static/css/documents.css
  - internal/web/static/css/system.css
  - internal/web/templates/ui.go
  - internal/web/templates/ui_test.go
consumes: ["routeTable (routes.go)", "handleHome, homeAction, homeActions (routes.go)", "statusChips (templates/ui.go)", "UI-03a stylesheet slot pattern"]
produces: ["Server.dashboardRoutes, findingsListRoutes, proposalsRoutes, documentsRoutes, systemRoutes, auditRoutes: each returns []route (empty today) and is appended by routeTable", "home.go holding handleHome and the home actions, moved unchanged", "empty stylesheets dashboard, findings-list, proposals, audit, documents, system imported by app.css", "StatusChip knows the partial status"]
acceptance:
  - go test ./internal/web/... -race -count=1
  - go test ./internal/web/templates/ ./internal/web/ -race -count=1 -run 'TestRouteHooks|TestUIComponentBadges|TestUIChipPartial|TestHomeActions|TestDesign'
  - test "$(go test ./internal/web/ ./internal/web/templates/ -list 'TestRouteHooks|TestUIChipPartial' | grep -c '^Test')" -ge 4
  - go vet ./internal/web/...
gates: [G1, G2, G3]
budget: {max_attempts: 3, max_wall_minutes: 90}
---

# UI-03b · Route hooks, page stylesheet slots and the partial chip

## Goal

Let the next page tickets (UI-04 dashboard, UI-06 findings list, UI-09 proposals and approvals, UI-10 audit, UI-11 documents, UI-12 system) be built in parallel: each adds its routes in its own new file instead of editing the shared route table, has its own empty stylesheet, and the home handler lives in its own file. Also teach the status chip about the `partial` run status.

## Context

- Same idea as UI-03a: those tickets would otherwise all edit `routes.go` (and UI-04 would rewrite `handleHome` inside it), which G0 refuses for parallel work.
- **No behaviour change.** The route table must be identical after this ticket: same methods, paths, roles and handlers, in the same order. The empty hooks add nothing. `handleHome`, `homeAction`, `homeActions` move verbatim to `home.go` (same package, same names, same behaviour; only the file changes).
- UI-08 is a dependency because it also edits `routes.go` (the `/ask` rows): this ticket starts from `main` after UI-08 merged.
- CSP and the escaping rules from UI-01/02 apply: no inline style or script.

## What to do

1. **Route hooks.** In `routes.go`, after the existing rows and before the `devComponents` block, append the rows of six hook methods, each defined in its own file as `func (s *Server) <name>Routes() []route { return nil }` with a one-line comment naming the ticket that fills it: `dashboardRoutes` (`routes_dashboard.go`, UI-04), `findingsListRoutes` (`routes_findings.go`, UI-06), `proposalsRoutes` (`routes_proposals.go`, UI-09), `documentsRoutes` (`routes_documents.go`, UI-11), `systemRoutes` (`routes_system.go`, UI-12), `auditRoutes` (`routes_audit.go`, UI-10). The order of the appends is fixed as listed here. A ticket that fills a hook adds routes only through it; it must not edit `routes.go`.
2. **Move the home handler.** Cut `type homeAction`, `var homeActions` and `func (s *Server) handleHome` (with their comments) from `routes.go` into `home.go`. Nothing else in `routes.go` changes. `shell.go` (`homeDestinations`) keeps working untouched.
3. **Stylesheet slots.** Create `static/css/dashboard.css`, `findings-list.css`, `proposals.css`, `audit.css`, `documents.css`, `system.css`, each a single header comment (`/* Dashboard (UI-04). */` and so on), imported by `app.css` after the existing page slots. The UI-03a design test already covers every `css/*.css`.
4. **`partial` chip.** `statusChips` in `templates/ui.go` has no entry for `partial` (a run that finished with some failures). Add one: label "Partial", warn variant, with an icon from the sprite that suits it (`alert`). `TestUIComponentBadges` already sweeps the listed statuses: extend it (and add `TestUIChipPartial`) so `partial` renders a chip with the visible text "Partial" and the warn class. Do not change any other chip's output.
5. **Tests** in `routes_hooks_test.go` (package `web`):
   - `TestRouteHooksEmpty`: each of the six hooks returns no routes.
   - `TestRouteHooksTableUnchanged`: building the route table yields no duplicate `method path` keys, and every row has a non-nil handler. (Do not pin the full list: later tickets will add rows.)
   - `TestRouteHooksAppended`: a test-only server (use the existing helpers) whose hook output is simulated is not possible without a seam, so instead assert that `routeTable` calls each hook exactly once, by verifying the six hook method values are referenced in `routeTable` (read `routes.go` text from disk and check each name appears once) and that none of the six files declares anything but its hook.
   - `TestRouteHooksHomeMoved`: `routes.go` no longer declares `handleHome`, `homeActions` or `homeAction`, and `home.go` does (a source-text check), while `TestHomeActions` keeps passing unchanged.

## Code map

- `internal/web/routes.go:39` `routeTable`, `:133` `homeAction`, `:137` `homeActions`, `:151` `handleHome`; the `devComponents` append at the end of `routeTable`; `routeBodyLimits` stays in `routes.go`.
- `internal/web/shell.go:281` `homeDestinations` uses `homeActions` indirectly: confirm it still compiles untouched.
- `internal/web/templates/ui.go:203` `statusChips`, `:218` the lookup; `internal/web/templates/ui_test.go` `TestUIComponentBadges`.
- `internal/web/static/app.css`: the `@import` list.

## Out of scope

- Any new route or page, the dashboard itself, changing `navTable` or any role.

## Notes

- No new dependencies.
- If moving the home block changes an import in `routes.go`, fix the imports; do not leave unused ones.
