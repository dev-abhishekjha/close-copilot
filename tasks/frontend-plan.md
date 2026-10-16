# Frontend plan (draft, 2026-10-16)

Status: plan approved on the owner's delegated decisions (section 8). No specs or graph entries exist yet; `/spec` turns each row of the ticket breakdown into `tasks/graph.yaml` entries (tickets use their own `UI-xx` series, separate from the CC tickets). `docs/**` is protected, so these tickets live in this file, as the hardening tickets do in `tasks/hardening.md`.

## 1. Problem

The UI works but is a thin shell around the API:

- The home page lists API paths (`GET /api/close-runs/{id}`, `POST /api/ask`) as text. Nothing is a button. To use `/api/ask`, or to see a run, you edit the URL by hand.
- No page for asking a question at all (`/api/ask` is JSON only).
- No global context: company and month are retyped on every form.
- No dashboard, no navigation, no proposals history, no status of the stack (ERPNext, Postgres, model), no documents browser.
- One 128-line stylesheet, no components, no empty/loading/error states, no responsive layout, no accessibility pass.

## 2. What good close tools do (research)

Sources: FloQast, Numeric, BlackLine product pages and comparisons; Xero's reconciliation redesign; maker-checker guidance. Vendor pages say little about layout, so these are patterns, not copies.

| Pattern | Seen in | Use here |
| --- | --- | --- |
| One close dashboard: status of every task, deadline slippage, blockers | FloQast, Numeric | Dashboard: current run per company, findings by severity, what is waiting on whom |
| Close as an ordered checklist with dependencies and sign-off per step | FloQast, Numeric | Run page shows the workflow stages (extract, checks, explain, verify) as a stepper with live status |
| Reconciliation as a side-by-side row: bank line next to ledger entry, suggested match, Match/Create/Discuss actions | Xero | Finding detail for bank charges: bank line vs proposed journal entry, one-click "Propose entry" |
| Compact vs comfortable density toggle | Xero | Density toggle on tables |
| AI drafts the explanation, human reviews it, nothing posts without approval | FloQast, Numeric | Explanation card with verified badge, citations, "needs review" banner (already modelled) |
| Maker-checker: visible status timeline, side-by-side before/after, reject with reason, independent checker, decision history | maker-checker guidance | Approvals queue and a proposal timeline (proposed, approved/rejected, posted) with actor and time |
| Clear status vocabulary (Pending, Under review, Approved, Posted, Rejected) | all | One status chip component used everywhere |

## 3. Decisions

1. **Stack: keep server-rendered Go `templ` + htmx; add a design system and small vanilla JS.** Reasons: the strict CSP (no inline script or style), the CSRF and session model, the role table in `routes.go`, one binary, and no Node toolchain all stay as they are. A React SPA would reopen every one of those for a 69-ticket project whose value is the agent, not the front end. Cost: highly interactive widgets (command palette, live charts) need hand-written JS. Fine at this scale. **Owner to confirm; see open questions.**
2. **Buttons come from the route table.** Today `homeActions` mirrors the API. Replace it with real UI: every API capability gets a page, form or button, rendered only when `s.allows(role, ...)` says so. The JSON API stays for scripts.
3. **Global context bar** (company + month), stored in a cookie, used as the default by every page.
4. **No external assets.** Self-hosted font, icons as an SVG sprite, charts as server-rendered SVG. Keeps CSP `default-src 'self'` and works offline in the demo.
5. **Every page gets four states:** loading, empty, error, populated. A page without all four does not pass review.
6. **Live updates** via htmx polling first (already used for `/runs/{id}/progress`), SSE only if polling proves too slow.

## 4. Information architecture

```
Top bar: logo | company ▾ | month ▾ | search / command palette (Ctrl+K) | health dot | user menu
Sidebar: Dashboard · Close runs · Findings · Ask · Approvals (checker) · Proposals · Documents · Audit (checker) · Metrics · System
```

| Page | Route | Roles | Status |
| --- | --- | --- | --- |
| Login | `/login` | public | exists, restyle |
| Dashboard | `/` | all | rebuild (replaces the API-path list) |
| Close runs list + start | `/runs` | all; start = maker | exists, rework |
| Run detail with stepper, live progress | `/runs/{id}` | all | exists, rework |
| Findings (all runs, filter, bulk) | `/findings`, `/runs/{id}/findings` | all | add cross-run view |
| Finding detail | `/findings/{id}` | all | exists, rework |
| Ask the books | `/ask` | all | **new** (history, citations, refusal display) |
| Approvals queue | `/approvals` | checker | exists, rework |
| Proposals (mine / all, timeline) | `/proposals`, `/proposals/{id}` | maker, checker | **new** |
| Documents / corpus browser | `/documents` | all | **new** (read-only, needs a list endpoint) |
| Audit log | `/audit` | checker | exists, rework |
| Run metrics | `/runs/{id}/metrics` | all | exists, add charts |
| System status | `/system` | all | **new** (Postgres, ERPNext, model provider, budget used) |

## 5. Design system

- Tokens in `app.css` `:root`: colour (light + dark via `prefers-color-scheme`), spacing scale, type scale, radius, shadow. Severity colours (high/medium/low) and status colours defined once, each with a text label so colour is never the only signal.
- Money in `₹` with Indian digit grouping (1,23,456.78), right-aligned, tabular figures. Paise formatting stays in one Go helper.
- Components (templ): `AppShell`, `Sidebar`, `ContextBar`, `Button` (primary, secondary, danger, loading), `StatusChip`, `SeverityBadge`, `VerifiedBadge`, `Card`, `DataTable` (sort, filter, pagination, density), `Tabs`, `Stepper`, `Toast`, `Modal/confirm`, `EmptyState`, `Skeleton`, `FormField`, `Stat tile`, `Timeline`, `CitationPopover`, `EvidenceTable`.
- Confirm dialogs for destructive and money actions (approve, reject, dismiss) with the amount and accounts restated.
- Accessibility: keyboard reachable everywhere, visible focus, `aria-live` for progress and toasts, contrast AA, skip link, labelled forms.
- Responsive down to tablet; phone is read-only friendly (no side-by-side).

## 6. Ticket breakdown (proposed)

Sizes: S ≤ 2 h, M ≤ 4 h (hand-coding estimate, as in the tickets doc). Owner is `implementer` unless noted. Risk class follows the existing graph; anything touching `routes.go` role rows or approvals stays `regulated`.

| ID | Title | Size | Depends on | Notes |
| --- | --- | --- | --- | --- |
| UI-01 | Design tokens, `app.css` rewrite, self-hosted font, icon sprite, dark mode | M | none | no inline style; CSP test stays green |
| UI-02 | Component library in templ (button, chip, badge, card, table, tabs, toast, modal, empty/skeleton) plus a `/dev/components` page behind a build flag | M | UI-01 | the page is the visual test |
| UI-03 | App shell: sidebar, top bar, context bar (company, month cookie), role-aware nav from the route table, breadcrumbs | M | UI-02 | replaces `homeActions` text list; `allows()` stays the authority |
| UI-04 | Dashboard: last run per company, severity counts, waiting-on-me (maker drafts, checker queue), quick actions (Start close, Ask) | M | UI-03 | needs one store query for cross-run counts |
| UI-05 | Close runs rework: start-close modal with company/month defaults, runs table with filters and status chips, stepper with live progress, **Re-run** button (same company and month, uses the existing start API; no cancel) | M | UI-03 | |
| UI-06 | Findings: cross-run list with filters (severity, type, status, verified), sort, bulk accept/dismiss | M | UI-03 | maker-only bulk actions reuse existing handlers |
| UI-07 | Finding detail rework: header facts, explanation card, citation popovers, evidence tables with ERPNext links, investigation trail, bank-line vs entry side-by-side, **Propose entry** button | M | UI-02 | |
| UI-08 | Ask page: question box, company/month from context, answer with citations and verified/needs-review badges, refusal and clarification states, per-session history | M | UI-03 | UI over `/api/ask`; no new model access |
| UI-09 | Proposals and approvals: proposals list, proposal timeline, checker queue with before/after, approve/reject confirm modals with reason, self-approval shown as disabled with the reason | M | UI-07 | regulated; ticket-reviewer runs; no change to `internal/approvals/**` |
| UI-10 | Audit and metrics rework: filter chips, export CSV, server-rendered SVG charts (cost per run, verified rate, tokens) | M | UI-02 | |
| UI-11 | Documents browser: list, filter by type, document view with sections; list endpoint on retrieval if missing | M | UI-03 | data-sensitive: company filter inside the SQL (CC-804); view pages show corpus text to humans only, never to a tool-calling model |
| UI-12 | System status page and top-bar health dot: Postgres, ERPNext, model provider, daily budget used | S | UI-03 | read-only; no secrets shown |
| UI-13 | Command palette (Ctrl+K), keyboard shortcuts, toasts and live-region announcements | S | UI-03 | vanilla JS file in `/static` |
| UI-14 | Accessibility and responsive pass, plus a visual and e2e test suite (Playwright against a seeded `agent serve`), screenshots stored per page | M | all pages | new dev dependency; spec notes it |

Order: UI-01, UI-02, UI-03 (foundation, strictly serial), then UI-04, UI-05, UI-07, UI-08 (the demo path), then UI-09, UI-06, UI-10, UI-11, UI-12, UI-13, then UI-14 as the closing gate.

Phase placement: a new **Phase 6 · Product UI** (UI-01 to UI-14), after Gate F, so nothing in Phases 0 to 5 moves. Gate G: every API row in the HTTP API table is reachable by a button for the role that may call it; viewer, maker and checker walkthroughs pass in the e2e suite; no CSP violations in the browser console.

## 7. Guardrails that carry over

- No inline script or style; `contentSecurityPolicy` is not loosened.
- CSRF token on every state-changing htmx request; the existing tests must stay green.
- `internal/web` still imports neither `internal/frappe` nor `internal/agent`; new data comes through the existing backend interfaces.
- Role checks stay in the route table. Hiding a button is a convenience, not security.
- Retrieved document text is rendered escaped and never sent to a model from the UI.
- Synthetic data only in screenshots and fixtures.
- Protected paths: this plan needs no edits to `internal/approvals/**`, `config/users.yaml`, `gates/**` or `.github/**`. If UI-14 wants CI wiring for Playwright, that is a separate owner-approved change.

## 8. Owner decisions (2026-10-16)

1. **Stack:** templ + htmx stays (owner delegated; my pick). No SPA.
2. **Scope:** the documents browser stays, because citations on a finding currently say "snippet not stored"; a reader needs to open the cited section to trust the answer. The API explorer is cut: every action gets a real button, and the JSON API is for scripts.
3. **Look:** owner has no reference; the design follows section 2 (calm, dense, Xero-like light theme, dark mode, Indian number formatting).
4. **Run actions:** the UI offers **Re-run** (start again with the same company and month, which the start API already supports). **Cancel** is out of scope; it needs a runner change and would be its own CC ticket.
5. **IDs:** separate `UI-xx` series; the tickets are not added to `docs/implementation-tickets.md`.

## 9. Harness ticket CC-021 (prerequisite)

Not in `docs/implementation-tickets.md`. The build harness accepts only `CC-<n>[a-z]` ticket IDs in the guard hook, the gates code, `/next` and two CI workflows. CC-021 teaches all of them `UI-<n>` (branch `ui-01-<slug>`, commit subject `UI-01: ...`, spec `specs/UI-01.md`) with tests, so the UI series can run on the normal `/spec` and `/build` path. The owner approved touching the protected `gates/**`, `.claude/**` and `.github/**` paths for it on 2026-10-16. Regulated: `ticket-reviewer` and G5 run before the merge. Every UI ticket depends on CC-021.
