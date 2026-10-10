package evals

// Recorded fixtures (CC-905). eval run --record wraps the live books and
// evidence readers (the agent's MCP readers) and writes each call's
// response to <dir>/<suite>/<company>-<month>/<tool>-<argshash>.json;
// eval run --replay serves those files through the same
// checks.BooksReader and checks.EvidenceReader interfaces, so CI runs the
// real close workflow with no ERPNext and no MCP server. The replay types
// hold no live reader: a call with no fixture is ErrFixtureMissing, never
// a live call.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// Fixture folder layout and format.
const (
	// DefaultFixturesDir is eval run's --fixtures-dir default.
	DefaultFixturesDir = "evals/fixtures"
	// FixtureIndexFile lists a suite's fixture files with their hashes.
	FixtureIndexFile = "index.json"
	// FixtureIndexVersion is the index format version.
	FixtureIndexVersion = 1
)

// The MCP tool each reader method maps to, as internal/agent's readers
// name them.
const (
	ToolTrialBalance       = "get_trial_balance"
	ToolGLEntries          = "list_gl_entries"
	ToolPurchaseInvoices   = "list_purchase_invoices"
	ToolSalesInvoices      = "list_sales_invoices"
	ToolPayments           = "list_payments"
	ToolAccountHistory     = "get_account_history"
	ToolRecurringSuppliers = "list_recurring_suppliers"
	ToolBankLines          = "list_bank_lines"
	ToolGSTR2BEntries      = "list_gstr2b_entries"
)

var (
	// ErrFixtureMissing is a replayed call with no recorded fixture.
	ErrFixtureMissing = errors.New("evals: fixture missing")
	// ErrFixturesStale is a replay whose suite or ground truth changed
	// since the fixtures were recorded.
	ErrFixturesStale = errors.New("evals: fixtures are stale: re-record after seeding (make record-fixtures)")
	// ErrFixturesInvalid is a fixture folder that doesn't match its index,
	// or a fixture that doesn't match its call.
	ErrFixturesInvalid = errors.New("evals: fixtures invalid")
	// ErrRecordingAborted is a recording stopped by a live error.
	ErrRecordingAborted = errors.New("evals: recording aborted")
)

// FixtureIndex is <dir>/<suite>/index.json.
type FixtureIndex struct {
	Version     int    `json:"version"`
	Suite       string `json:"suite"`
	SuiteSHA256 string `json:"suite_sha256"`
	// Truth maps each recorded month's ground-truth file name
	// (<company>-<month>.json) to its sha256.
	Truth map[string]string `json:"truth"`
	// Months are the recorded months, <company>-<month>, sorted.
	Months []string `json:"months"`
	// SeederCommit is the build (buildinfo) that recorded the fixtures.
	SeederCommit string    `json:"seeder_commit"`
	RecordedAt   time.Time `json:"recorded_at"`
	// Files maps each fixture's path under the suite folder
	// (<company>-<month>/<tool>-<argshash>.json) to its sha256.
	Files map[string]string `json:"files"`
}

// fixtureFile is one fixture's body.
type fixtureFile struct {
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
	Result json.RawMessage `json:"result"`
}

// fixtureRelRe is the shape of a fixture's path under the suite folder.
var fixtureRelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}-[0-9]{4}-[0-9]{2}/[a-z0-9_]{1,64}-[0-9a-f]{16}\.json$`)

// dateArg is a date argument as YYYY-MM-DD, "" for the zero time.
func dateArg(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.DateOnly)
}

// canonicalArgs is the canonical JSON of a call's arguments: sorted keys,
// no spaces, dates as YYYY-MM-DD and months as YYYY-MM (as given).
func canonicalArgs(args map[string]any) ([]byte, error) {
	b, err := json.Marshal(args) // maps encode with sorted keys
	if err != nil {
		return nil, fmt.Errorf("evals: fixture args: %w", err)
	}
	return b, nil
}

// recanonical re-encodes stored args JSON canonically.
func recanonical(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("args are not an object")
	}
	return json.Marshal(m)
}

// argsHash is the first 16 hex characters of the sha256 of canonical args.
func argsHash(canon []byte) string {
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])[:16]
}

// fixtureRel is the fixture path of a call under the suite folder.
func fixtureRel(scope, tool string, canon []byte) string {
	return scope + "/" + tool + "-" + argsHash(canon) + ".json"
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fileSHA256 is the sha256 of a file's contents.
func fileSHA256(p string) (string, error) {
	b, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// MonthScope is told which company-month a close is about to run; a
// fixture is filed under, and served from, that month's folder.
type MonthScope interface {
	UseMonth(company, month string)
}

// ScopedCloser sets Scope to each month before its close runs. After, if
// set, runs once the close returns (a recording primes the month's trial
// balance with it).
type ScopedCloser struct {
	Closer Closer
	Scope  MonthScope
	After  func(ctx context.Context, company, month string) error
}

// RunClose runs one close in the month's fixture scope.
func (c *ScopedCloser) RunClose(ctx context.Context, company, month string) (agent.Result, error) {
	c.Scope.UseMonth(company, month)
	res, err := c.Closer.RunClose(ctx, company, month)
	if c.After != nil {
		if aerr := c.After(ctx, company, month); aerr != nil {
			err = errors.Join(err, aerr)
		}
	}
	return res, err
}

// ---- recording ----

// Recorder writes one suite's fixtures into a staging folder; Finish
// scans them, writes the index and swaps the folder in for
// <dir>/<suite>. A live error aborts the recording: errors are never
// recorded, every later call fails, and Finish refuses.
type Recorder struct {
	root, suite, stage string

	mu    sync.Mutex
	scope string            // <company>-<month>
	files map[string]string // fixture path -> sha256
	err   error
	done  bool
}

// NewRecorder starts a recording of suite under root.
func NewRecorder(root, suite string) (*Recorder, error) {
	if !nameRe.MatchString(suite) {
		return nil, fmt.Errorf("evals: record: %.64q is not a suite name", suite)
	}
	if err := CheckOutputPath("--fixtures-dir", root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("evals: record: %w", err)
	}
	stage, err := os.MkdirTemp(root, "."+suite+".recording-")
	if err != nil {
		return nil, fmt.Errorf("evals: record: %w", err)
	}
	return &Recorder{root: root, suite: suite, stage: stage, files: map[string]string{}}, nil
}

// UseMonth files the next calls under company-month.
func (r *Recorder) UseMonth(company, month string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scope = company + "-" + month
}

// Err is the error that aborted the recording, nil while it is good.
func (r *Recorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Abort removes the staging folder. It is a no-op after Finish.
func (r *Recorder) Abort() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return
	}
	r.done = true
	_ = os.RemoveAll(r.stage)
}

// Books wraps a live books reader.
func (r *Recorder) Books(live checks.BooksReader) *RecordingBooks {
	return &RecordingBooks{rec: r, Live: live}
}

// Evidence wraps a live evidence reader.
func (r *Recorder) Evidence(live checks.EvidenceReader) *RecordingEvidence {
	return &RecordingEvidence{rec: r, Live: live}
}

// before fails a call once the recording is aborted or finished.
func (r *Recorder) before() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return fmt.Errorf("%w: %w", ErrRecordingAborted, r.err)
	}
	if r.done {
		return fmt.Errorf("%w: the recording is finished", ErrRecordingAborted)
	}
	return nil
}

// abort records the first error that stops the recording.
func (r *Recorder) abort(err error) error {
	if r.err == nil {
		r.err = err
	}
	return err
}

// write files one call's result. The live error, if any, aborts the
// recording and is returned unchanged.
func (r *Recorder) write(tool string, args map[string]any, result any, liveErr error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if liveErr != nil {
		_ = r.abort(fmt.Errorf("live %s failed: %w", tool, liveErr))
		return liveErr
	}
	if r.err != nil {
		return fmt.Errorf("%w: %w", ErrRecordingAborted, r.err)
	}
	if r.scope == "" {
		return r.abort(fmt.Errorf("%w: %s called with no month in scope", ErrRecordingAborted, tool))
	}
	canon, err := canonicalArgs(args)
	if err != nil {
		return r.abort(err)
	}
	argsRaw := json.RawMessage(canon)
	resRaw, err := json.Marshal(result)
	if err != nil {
		return r.abort(fmt.Errorf("evals: record %s: encode result: %w", tool, err))
	}
	body, err := marshalSorted(fixtureFile{Tool: tool, Args: argsRaw, Result: resRaw})
	if err != nil {
		return r.abort(err)
	}
	rel := fixtureRel(r.scope, tool, canon)
	sum := sha256Hex(body)
	if prev, ok := r.files[rel]; ok {
		if prev != sum {
			return r.abort(fmt.Errorf("%w: %s %s returned different results for the same call in %s; replay would not be deterministic",
				ErrRecordingAborted, tool, canon, r.scope))
		}
		return nil
	}
	p := filepath.Join(r.stage, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return r.abort(fmt.Errorf("evals: record: %w", err))
	}
	if err := writeFileAtomic(p, body); err != nil {
		return r.abort(err)
	}
	r.files[rel] = sum
	return nil
}

// recordCall runs a live call unless the recording is aborted and files
// its result.
func recordCall[T any](r *Recorder, tool string, args map[string]any, call func() (T, error)) (T, error) {
	var zero T
	if err := r.before(); err != nil {
		return zero, err
	}
	v, err := call()
	if werr := r.write(tool, args, v, err); werr != nil {
		return zero, werr
	}
	return v, nil
}

// RecordMeta is what Finish writes into the index besides the files.
type RecordMeta struct {
	Suite Suite
	// TruthDir holds the suite's ground truth (<company>-<month>.json).
	TruthDir string
	// Months are the months the recording ran.
	Months       []SuiteMonth
	SeederCommit string
	RecordedAt   time.Time
	// Profiles and ExtraAllowed are the scan's identifier allowlist
	// (ScanFixtures).
	Profiles     []company.Profile
	ExtraAllowed []string
}

// Finish checks the recording, scans it for secrets and stray
// identifiers, writes the index and replaces <dir>/<suite> with it. It
// returns the index and the index file's sha256. On any error the
// staging folder is removed and <dir>/<suite> is left as it was.
func (r *Recorder) Finish(ctx context.Context, meta RecordMeta) (FixtureIndex, string, error) {
	idx, sum, err := r.finish(ctx, meta)
	if err != nil {
		r.Abort()
		return FixtureIndex{}, "", err
	}
	return idx, sum, nil
}

func (r *Recorder) finish(ctx context.Context, meta RecordMeta) (FixtureIndex, string, error) {
	r.mu.Lock()
	switch {
	case r.err != nil:
		err := fmt.Errorf("%w: %w", ErrRecordingAborted, r.err)
		r.mu.Unlock()
		return FixtureIndex{}, "", err
	case r.done:
		r.mu.Unlock()
		return FixtureIndex{}, "", fmt.Errorf("%w: the recording is finished", ErrRecordingAborted)
	}
	files := make(map[string]string, len(r.files))
	for k, v := range r.files {
		files[k] = v
	}
	r.mu.Unlock()

	if meta.Suite.Name != r.suite {
		return FixtureIndex{}, "", fmt.Errorf("evals: record: suite %s, recording %s", meta.Suite.Name, r.suite)
	}
	if len(meta.Months) == 0 {
		return FixtureIndex{}, "", errors.New("evals: record: no months")
	}
	idx := FixtureIndex{
		Version: FixtureIndexVersion, Suite: r.suite, SuiteSHA256: meta.Suite.SHA256,
		Truth: map[string]string{}, Months: []string{}, SeederCommit: meta.SeederCommit,
		RecordedAt: meta.RecordedAt.UTC(), Files: files,
	}
	for _, m := range meta.Months {
		name := TruthFileName(m.Company, m.Month)
		sum, err := fileSHA256(filepath.Join(meta.TruthDir, name))
		if err != nil {
			return FixtureIndex{}, "", fmt.Errorf("evals: record: ground truth for %s: %w", m.Key(), err)
		}
		idx.Truth[name] = sum
		idx.Months = append(idx.Months, m.Company+"-"+m.Month)
	}
	slices.Sort(idx.Months)

	if err := ScanFixtures(ctx, r.stage, meta.Profiles, meta.ExtraAllowed); err != nil {
		return FixtureIndex{}, "", err
	}
	body, err := marshalSorted(idx)
	if err != nil {
		return FixtureIndex{}, "", err
	}
	if err := writeFileAtomic(filepath.Join(r.stage, FixtureIndexFile), body); err != nil {
		return FixtureIndex{}, "", err
	}
	if err := r.swap(); err != nil {
		return FixtureIndex{}, "", err
	}
	return idx, sha256Hex(body), nil
}

// swap replaces <root>/<suite> with the staging folder.
func (r *Recorder) swap() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	final := filepath.Join(r.root, r.suite)
	old := ""
	if _, err := os.Lstat(final); err == nil {
		o, err := os.MkdirTemp(r.root, "."+r.suite+".old-")
		if err != nil {
			return fmt.Errorf("evals: record: %w", err)
		}
		old = filepath.Join(o, r.suite)
		if err := os.Rename(final, old); err != nil {
			_ = os.Remove(o)
			return fmt.Errorf("evals: record: move the old fixtures aside: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("evals: record: %w", err)
	}
	if err := os.Rename(r.stage, final); err != nil {
		if old != "" {
			_ = os.Rename(old, final)
			_ = os.Remove(filepath.Dir(old))
		}
		return fmt.Errorf("evals: record: %w", err)
	}
	r.done = true
	if old != "" {
		_ = os.RemoveAll(filepath.Dir(old))
	}
	return nil
}

// RecordingBooks is a live books reader whose every result is recorded.
type RecordingBooks struct {
	rec  *Recorder
	Live checks.BooksReader
}

var _ checks.BooksReader = (*RecordingBooks)(nil)

// TrialBalance records get_trial_balance.
func (b *RecordingBooks) TrialBalance(ctx context.Context, company string, from, to time.Time) (ledger.TB, error) {
	return recordCall(b.rec, ToolTrialBalance, periodArgs(company, from, to), func() (ledger.TB, error) {
		return b.Live.TrialBalance(ctx, company, from, to)
	})
}

// GLEntries records list_gl_entries.
func (b *RecordingBooks) GLEntries(ctx context.Context, company string, from, to time.Time) ([]ledger.GLEntry, error) {
	return recordCall(b.rec, ToolGLEntries, periodArgs(company, from, to), func() ([]ledger.GLEntry, error) {
		return b.Live.GLEntries(ctx, company, from, to)
	})
}

// PurchaseInvoices records list_purchase_invoices.
func (b *RecordingBooks) PurchaseInvoices(ctx context.Context, company string, from, to time.Time) ([]ledger.PurchaseInvoice, error) {
	return recordCall(b.rec, ToolPurchaseInvoices, periodArgs(company, from, to), func() ([]ledger.PurchaseInvoice, error) {
		return b.Live.PurchaseInvoices(ctx, company, from, to)
	})
}

// SalesInvoices records list_sales_invoices.
func (b *RecordingBooks) SalesInvoices(ctx context.Context, company string, from, to time.Time) ([]ledger.SalesInvoice, error) {
	return recordCall(b.rec, ToolSalesInvoices, periodArgs(company, from, to), func() ([]ledger.SalesInvoice, error) {
		return b.Live.SalesInvoices(ctx, company, from, to)
	})
}

// PaymentEntries records list_payments.
func (b *RecordingBooks) PaymentEntries(ctx context.Context, company string, from, to time.Time) ([]ledger.PaymentEntry, error) {
	return recordCall(b.rec, ToolPayments, periodArgs(company, from, to), func() ([]ledger.PaymentEntry, error) {
		return b.Live.PaymentEntries(ctx, company, from, to)
	})
}

// AccountHistory records get_account_history.
func (b *RecordingBooks) AccountHistory(ctx context.Context, company, account string, through string, months int) ([]ledger.MonthTotal, error) {
	return recordCall(b.rec, ToolAccountHistory, accountHistoryArgs(company, account, through, months), func() ([]ledger.MonthTotal, error) {
		return b.Live.AccountHistory(ctx, company, account, through, months)
	})
}

// RecurringSuppliers records list_recurring_suppliers.
func (b *RecordingBooks) RecurringSuppliers(ctx context.Context, company string, beforeMonth string, lookbackMonths int, minOccurrences int, amountBandPct int) ([]checks.RecurringSupplier, error) {
	args := recurringArgs(company, beforeMonth, lookbackMonths, minOccurrences, amountBandPct)
	return recordCall(b.rec, ToolRecurringSuppliers, args, func() ([]checks.RecurringSupplier, error) {
		return b.Live.RecurringSuppliers(ctx, company, beforeMonth, lookbackMonths, minOccurrences, amountBandPct)
	})
}

// PrimeMonth records the month's trial balance, the one call the
// explainer makes (agent.BooksAccounts), so a recording made without the
// model still serves a Tier 2 replay.
func (b *RecordingBooks) PrimeMonth(ctx context.Context, companyID, month string) error {
	from, err := company.ParseMonth(month)
	if err != nil {
		return fmt.Errorf("evals: record: %w", err)
	}
	_, err = b.TrialBalance(ctx, companyID, from, from.AddDate(0, 1, -1))
	return err
}

// RecordingEvidence is a live evidence reader whose every result is
// recorded.
type RecordingEvidence struct {
	rec  *Recorder
	Live checks.EvidenceReader
}

var _ checks.EvidenceReader = (*RecordingEvidence)(nil)

// BankLines records list_bank_lines.
func (e *RecordingEvidence) BankLines(ctx context.Context, company string, from, to time.Time) ([]store.BankLine, error) {
	return recordCall(e.rec, ToolBankLines, periodArgs(company, from, to), func() ([]store.BankLine, error) {
		return e.Live.BankLines(ctx, company, from, to)
	})
}

// GSTR2BEntries records list_gstr2b_entries.
func (e *RecordingEvidence) GSTR2BEntries(ctx context.Context, company string, period string) ([]store.GSTR2BEntry, error) {
	return recordCall(e.rec, ToolGSTR2BEntries, gstr2bArgs(company, period), func() ([]store.GSTR2BEntry, error) {
		return e.Live.GSTR2BEntries(ctx, company, period)
	})
}

func periodArgs(company string, from, to time.Time) map[string]any {
	return map[string]any{"company": company, "from": dateArg(from), "to": dateArg(to)}
}

func accountHistoryArgs(company, account, through string, months int) map[string]any {
	return map[string]any{"company": company, "account": account, "through": through, "months": months}
}

func recurringArgs(company, beforeMonth string, lookback, minOcc, band int) map[string]any {
	return map[string]any{"company": company, "before_month": beforeMonth, "lookback_months": lookback,
		"min_occurrences": minOcc, "amount_band_pct": band}
}

func gstr2bArgs(company, period string) map[string]any {
	return map[string]any{"company": company, "period": period}
}

// ---- replay ----

// Replayer serves one suite's fixtures, read and checked against the
// index once, when it is opened.
type Replayer struct {
	index  FixtureIndex
	bodies map[string][]byte // fixture path -> body

	mu    sync.Mutex
	scope string
}

// ReplayOptions locate a replay's fixtures and what they must still match.
type ReplayOptions struct {
	// Dir is the fixtures root; the suite's fixtures are under Dir/<suite>.
	Dir   string
	Suite Suite
	// TruthDir holds the suite's ground truth now.
	TruthDir string
	// Months are the months about to run; each must be recorded.
	Months []SuiteMonth
}

// OpenReplay reads <dir>/<suite>: the index, then every file. It fails if
// a file's hash differs from the index, a listed file is missing, any
// file is not listed (an extra file is an error), or the suite file or a
// recorded ground-truth file changed since recording (ErrFixturesStale),
// before any month runs. It returns the replayer and the index sha256.
func OpenReplay(opts ReplayOptions) (*Replayer, string, error) {
	dir := filepath.Join(opts.Dir, opts.Suite.Name)
	indexPath := filepath.Join(dir, FixtureIndexFile)
	raw, err := os.ReadFile(filepath.Clean(indexPath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("%w: no fixtures index %s: record the suite first (make record-fixtures SUITE=%s)", ErrFixtureMissing, indexPath, opts.Suite.Name)
		}
		return nil, "", fmt.Errorf("evals: replay: %w", err)
	}
	var idx FixtureIndex
	if err := decodeStrict(raw, &idx); err != nil {
		return nil, "", fmt.Errorf("%w: %s: %w", ErrFixturesInvalid, indexPath, err)
	}
	switch {
	case idx.Version != FixtureIndexVersion:
		return nil, "", fmt.Errorf("%w: %s: version %d, want %d", ErrFixturesInvalid, indexPath, idx.Version, FixtureIndexVersion)
	case idx.Suite != opts.Suite.Name:
		return nil, "", fmt.Errorf("%w: %s is for suite %.64q, not %s", ErrFixturesInvalid, indexPath, idx.Suite, opts.Suite.Name)
	case idx.Files == nil || idx.Truth == nil:
		return nil, "", fmt.Errorf("%w: %s lists no files or no truth", ErrFixturesInvalid, indexPath)
	}

	rp := &Replayer{index: idx, bodies: map[string][]byte{}}
	var errs []error
	present := map[string]bool{}
	// Walk and read through an os.Root: nothing outside the suite folder
	// is reachable, through .. or a symlink.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", fmt.Errorf("evals: replay: %w", err)
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		switch {
		case d.IsDir():
			return nil
		case rel == FixtureIndexFile:
			return nil
		case !d.Type().IsRegular():
			errs = append(errs, fmt.Errorf("%s is not a regular file", rel))
			return nil
		}
		present[rel] = true
		want, ok := idx.Files[rel]
		if !ok {
			errs = append(errs, fmt.Errorf("%s is not listed in the index", rel))
			return nil
		}
		body, err := root.ReadFile(rel)
		if err != nil {
			return err
		}
		if got := sha256Hex(body); got != want {
			errs = append(errs, fmt.Errorf("%s has sha256 %s, the index says %s", rel, got, want))
			return nil
		}
		rp.bodies[rel] = body
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("evals: replay: %w", err)
	}
	for _, rel := range sortedKeys(idx.Files) {
		if !fixtureRelRe.MatchString(rel) {
			errs = append(errs, fmt.Errorf("index entry %.120q is not <company>-<month>/<tool>-<hash>.json", rel))
			continue
		}
		if !present[rel] {
			errs = append(errs, fmt.Errorf("%s is listed in the index but missing", rel))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, "", fmt.Errorf("%w: %s: %w", ErrFixturesInvalid, dir, err)
	}

	// The suite and the ground truth must be what was recorded.
	var stale []string
	if idx.SuiteSHA256 != opts.Suite.SHA256 {
		stale = append(stale, fmt.Sprintf("suite %s changed", opts.Suite.Path))
	}
	for _, name := range sortedKeys(idx.Truth) {
		got, err := fileSHA256(filepath.Join(opts.TruthDir, name))
		switch {
		case err != nil:
			stale = append(stale, fmt.Sprintf("ground truth %s: %v", name, err))
		case got != idx.Truth[name]:
			stale = append(stale, fmt.Sprintf("ground truth %s changed", name))
		}
	}
	for _, m := range opts.Months {
		key := m.Company + "-" + m.Month
		if !slices.Contains(idx.Months, key) {
			stale = append(stale, fmt.Sprintf("month %s was not recorded", key))
		} else if _, ok := idx.Truth[TruthFileName(m.Company, m.Month)]; !ok {
			stale = append(stale, fmt.Sprintf("month %s has no recorded ground truth hash", key))
		}
	}
	if len(stale) > 0 {
		return nil, "", fmt.Errorf("%w: %v", ErrFixturesStale, stale)
	}
	return rp, sha256Hex(raw), nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Index is the index the replayer was opened with.
func (p *Replayer) Index() FixtureIndex { return p.index }

// UseMonth serves the next calls from company-month's folder.
func (p *Replayer) UseMonth(company, month string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scope = company + "-" + month
}

// Books is the replaying books reader.
func (p *Replayer) Books() *ReplayBooks { return &ReplayBooks{rp: p} }

// Evidence is the replaying evidence reader.
func (p *Replayer) Evidence() *ReplayEvidence { return &ReplayEvidence{rp: p} }

// load decodes the fixture of one call into out. The stored tool and args
// must equal the call's (a hash collision is an error), and the result
// must decode strictly.
func (p *Replayer) load(tool string, args map[string]any, out any) error {
	p.mu.Lock()
	scope := p.scope
	p.mu.Unlock()
	canon, err := canonicalArgs(args)
	if err != nil {
		return err
	}
	if scope == "" {
		return fmt.Errorf("%w: %s %s called with no month in scope", ErrFixtureMissing, tool, canon)
	}
	rel := fixtureRel(scope, tool, canon)
	body, ok := p.bodies[rel]
	if !ok {
		return fmt.Errorf("%w: %s %s for %s (%s); re-record (make record-fixtures)", ErrFixtureMissing, tool, canon, scope, rel)
	}
	var f fixtureFile
	if err := decodeStrict(body, &f); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrFixturesInvalid, rel, err)
	}
	if f.Tool != tool {
		return fmt.Errorf("%w: %s holds tool %.64q, not %s", ErrFixturesInvalid, rel, f.Tool, tool)
	}
	stored, err := recanonical(f.Args)
	if err != nil {
		return fmt.Errorf("%w: %s: args: %w", ErrFixturesInvalid, rel, err)
	}
	if !bytes.Equal(stored, canon) {
		return fmt.Errorf("%w: %s holds args %s, not %s", ErrFixturesInvalid, rel, stored, canon)
	}
	if err := decodeStrict(f.Result, out); err != nil {
		return fmt.Errorf("%w: %s: result: %w", ErrFixturesInvalid, rel, err)
	}
	return nil
}

// decodeStrict decodes JSON into v, rejecting unknown fields and
// trailing data.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func replay[T any](p *Replayer, tool string, args map[string]any) (T, error) {
	var v T
	if err := p.load(tool, args, &v); err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}

// ReplayBooks serves recorded books calls. It holds no live reader.
type ReplayBooks struct {
	rp *Replayer
}

var _ checks.BooksReader = (*ReplayBooks)(nil)

// TrialBalance replays get_trial_balance.
func (b *ReplayBooks) TrialBalance(_ context.Context, company string, from, to time.Time) (ledger.TB, error) {
	return replay[ledger.TB](b.rp, ToolTrialBalance, periodArgs(company, from, to))
}

// GLEntries replays list_gl_entries.
func (b *ReplayBooks) GLEntries(_ context.Context, company string, from, to time.Time) ([]ledger.GLEntry, error) {
	return replay[[]ledger.GLEntry](b.rp, ToolGLEntries, periodArgs(company, from, to))
}

// PurchaseInvoices replays list_purchase_invoices.
func (b *ReplayBooks) PurchaseInvoices(_ context.Context, company string, from, to time.Time) ([]ledger.PurchaseInvoice, error) {
	return replay[[]ledger.PurchaseInvoice](b.rp, ToolPurchaseInvoices, periodArgs(company, from, to))
}

// SalesInvoices replays list_sales_invoices.
func (b *ReplayBooks) SalesInvoices(_ context.Context, company string, from, to time.Time) ([]ledger.SalesInvoice, error) {
	return replay[[]ledger.SalesInvoice](b.rp, ToolSalesInvoices, periodArgs(company, from, to))
}

// PaymentEntries replays list_payments.
func (b *ReplayBooks) PaymentEntries(_ context.Context, company string, from, to time.Time) ([]ledger.PaymentEntry, error) {
	return replay[[]ledger.PaymentEntry](b.rp, ToolPayments, periodArgs(company, from, to))
}

// AccountHistory replays get_account_history.
func (b *ReplayBooks) AccountHistory(_ context.Context, company, account string, through string, months int) ([]ledger.MonthTotal, error) {
	return replay[[]ledger.MonthTotal](b.rp, ToolAccountHistory, accountHistoryArgs(company, account, through, months))
}

// RecurringSuppliers replays list_recurring_suppliers.
func (b *ReplayBooks) RecurringSuppliers(_ context.Context, company string, beforeMonth string, lookbackMonths int, minOccurrences int, amountBandPct int) ([]checks.RecurringSupplier, error) {
	return replay[[]checks.RecurringSupplier](b.rp, ToolRecurringSuppliers,
		recurringArgs(company, beforeMonth, lookbackMonths, minOccurrences, amountBandPct))
}

// ReplayEvidence serves recorded evidence calls. It holds no live reader.
type ReplayEvidence struct {
	rp *Replayer
}

var _ checks.EvidenceReader = (*ReplayEvidence)(nil)

// BankLines replays list_bank_lines.
func (e *ReplayEvidence) BankLines(_ context.Context, company string, from, to time.Time) ([]store.BankLine, error) {
	return replay[[]store.BankLine](e.rp, ToolBankLines, periodArgs(company, from, to))
}

// GSTR2BEntries replays list_gstr2b_entries.
func (e *ReplayEvidence) GSTR2BEntries(_ context.Context, company string, period string) ([]store.GSTR2BEntry, error) {
	return replay[[]store.GSTR2BEntry](e.rp, ToolGSTR2BEntries, gstr2bArgs(company, period))
}
