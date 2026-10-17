# Browser suite (UI-14)

Playwright tests for the web UI: the viewer, maker and checker walkthroughs, an
axe accessibility scan of every page, keyboard and responsive checks, a
Content-Security-Policy watch and one screenshot per page.

The suite runs against an **in-memory fixture server** (`e2e/fixture`): no
Docker, no Postgres, no ERPNext, no model, no network. Node and its packages
live only under `e2e/` and only `make e2e` needs them; `make check` and
`make gates` never do.

## Run it

From the repository root:

```sh
make e2e                                   # everything, about a minute
make e2e E2E_ARGS=a11y.spec.ts             # one spec
make e2e E2E_ARGS='-g "tablet"'            # by title
make e2e-shots                             # only the screenshots
```

`make e2e` installs the packages with `npm ci --prefix e2e`, installs Chromium
only if it is missing, starts the fixture (`go run ./e2e/cmd/e2eserver`, on
`127.0.0.1:8099`) through Playwright's `webServer`, and writes the log to
`tmp/reports/e2e.log`. On failure it prints the failing lines.

Dev dependencies, pinned exactly in `package.json`: `@playwright/test` 1.64.1
and `@axe-core/playwright` 4.14.0.

The Go side of the fixture has its own tests: `go test ./e2e/...`.

## The fixture and its users

| User | Password | Role |
| --- | --- | --- |
| `viewer` | `e2e-viewer-password` | viewer |
| `asha` | `e2e-maker-password` | maker |
| `ravi` | `e2e-checker-password` | checker |

These are synthetic and safe: the fixture holds them only in memory, binds to a
loopback address and refuses to start on any other (`e2eserver` checks
`E2E_ADDR`), and serves non-Secure cookies because it is plain http on
localhost. They match nothing in `config/users.yaml`. Never reuse them anywhere.

The data is seeded in `fixture/seed.go`: two invented companies (`mehta`,
`sharma`), a finished run with the three planted bank charges (one verified with
citations, one needing review, one with a proposed entry), a failed run, a queued
run, an older run with three pending proposals, one posted and one rejected, a
few audit rows, documents of each type and a healthy System page. Asking about
the bank gets a cited answer; any other question gets the refusal.

One server holds the state, so the specs run one after the other and in file
order: the read-only specs (`a11y`, `csp`, `keyboard`, `responsive`,
`screenshots`) come before the walkthroughs, which write (a run is started, a
proposal is approved and another rejected). Restarting the server (every
`make e2e`) resets everything. A restart also invalidates sessions, so the
stored sign-ins in `.auth/` are rebuilt in `global-setup.ts` each time.

## Layout

- `playwright.config.ts`: Chromium only, the viewports (`desktop` 1280x800,
  `tablet` 820x1180, `phone` 390x844) and the `webServer`.
- `tests/guard.ts`: runs behind every test (except `a11y.spec.ts`, see below)
  and fails it on a CSP violation, a console error, an uncaught page error or a
  request to another origin.
- `tests/pages.ts`: the list of pages and the roles that may open them.
- `tests/*.spec.ts`: the specs.

`a11y.spec.ts` bypasses the CSP because axe injects style elements; the CSP is
watched by every other spec. No axe rule is disabled.

## Add a page

1. Add the page to `fixture/seed.go` if it needs data, and to the `pages` list
   in `e2e/fixture/fixture_test.go` with the roles that may open it.
2. Add a row to `pages` in `tests/pages.ts`. The axe, CSP, responsive and
   screenshot specs pick it up from there.
3. If the page has a form or a button, add it to `TestEveryPostRouteHasAForm` in
   `fixture/fixture_test.go` (it fails on a POST route it does not know) and to
   the walkthrough of the role that uses it.
4. `make e2e-shots` and commit the new `screenshots/<name>.png`.

## Refresh the screenshots

`make e2e-shots`, then look at a few of the changed images before committing
them. They are one PNG per page in `screenshots/`.
