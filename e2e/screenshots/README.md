# Screenshots

One full-page PNG per page of the web UI, at 1280x800, written by
`tests/screenshots.spec.ts`. They are documentation, not a visual baseline:
nothing diffs against them.

Everything in them is synthetic. The data comes from the in-memory fixture
(`e2e/fixture`): invented companies, invented bank charges, no real GSTINs, no
real people.

Refresh them from the repository root:

```sh
make e2e-shots
```

The run fixes the browser clock and turns animations and carets off, so a
re-run with no UI change rewrites near-identical images.
