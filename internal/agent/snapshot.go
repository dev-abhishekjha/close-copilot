package agent

// Snapshotting readers (CC-709). SnapshotBooks and SnapshotEvidence wrap any
// checks.BooksReader or checks.EvidenceReader (the MCP readers of CC-702 or
// a fake) and store every result as a tool_result artifact whose content is
// {server, tool, args, result}: the MCP tool name, its arguments under the
// tool's own argument names, and the result the reader returned. After a
// check step, Annotate fills each finding's EvidenceRef.Artifact with the
// address of the snapshot that holds the evidence, so the verifier and
// cmd/audit read stored snapshots, never live data.
//
// The agent only keeps addresses and passes them on: it never reads
// artifact content back, and nothing here logs it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// ArtifactPutter stores a value as a content-addressed artifact and returns
// its sha256. *store.Store implements it.
type ArtifactPutter interface {
	PutArtifact(ctx context.Context, kind string, runID, stepID uuid.UUID, v any) (string, error)
}

var _ ArtifactPutter = (*store.Store)(nil)

// ErrNoSnapshot reports a finding's evidence ref that no recorded tool
// result supports.
var ErrNoSnapshot = errors.New("agent: no snapshot for evidence ref")

// toolSnapshot is the content of a tool_result artifact.
type toolSnapshot struct {
	Server string         `json:"server"`
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args"`
	Result any            `json:"result"`
}

// snapshotCall is what Snapshots remembers about one stored tool result:
// never the content, only its address and the identifiers it holds.
type snapshotCall struct {
	server string
	tool   string
	args   map[string]string // canonical JSON of each argument value
	sha    string
	// keys maps an identifier field (txn_id, voucher_no, name, ...) to the
	// values present in the result.
	keys map[string]map[string]bool
	// ids are the record identifiers an EvidenceRef.IDs entry may name.
	ids map[string]bool
}

// Snapshots records the tool results of one step. It is safe for
// concurrent use by the checks of that step.
type Snapshots struct {
	store  ArtifactPutter
	runID  uuid.UUID
	stepID uuid.UUID

	mu    sync.Mutex
	calls []snapshotCall
}

// NewSnapshots returns a recorder that stores tool results as artifacts of
// runID produced by stepID.
func NewSnapshots(st ArtifactPutter, runID, stepID uuid.UUID) *Snapshots {
	return &Snapshots{store: st, runID: runID, stepID: stepID}
}

// Refs returns the distinct artifact addresses recorded so far, in order,
// for the step's output_refs.
func (s *Snapshots) Refs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.calls))
	seen := map[string]bool{}
	for _, c := range s.calls {
		if !seen[c.sha] {
			seen[c.sha] = true
			out = append(out, c.sha)
		}
	}
	return out
}

// record stores one tool result and remembers its address. A storage
// failure fails the read: evidence that can't be snapshotted can't be used.
func (s *Snapshots) record(ctx context.Context, server, tool string, args map[string]any, result any, idx resultIndex) error {
	if s == nil || s.store == nil {
		return errors.New("agent: snapshot recorder has no store")
	}
	sha, err := s.store.PutArtifact(ctx, store.ArtifactToolResult, s.runID, s.stepID, toolSnapshot{
		Server: server, Tool: tool, Args: args, Result: result,
	})
	if err != nil {
		return fmt.Errorf("agent: snapshot %s%s%s: %w", server, toolSep, tool, err)
	}
	canon := make(map[string]string, len(args))
	for k, v := range args {
		b, err := store.Canonical(v)
		if err != nil {
			return fmt.Errorf("agent: snapshot %s%s%s: argument %s: %w", server, toolSep, tool, k, err)
		}
		canon[k] = string(b)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, snapshotCall{server: server, tool: tool, args: canon, sha: sha, keys: idx.keys, ids: idx.ids})
	return nil
}

// Annotate sets EvidenceRef.Artifact on every evidence ref of findings to
// the snapshot that supports it. Every ref must carry a company argument.
// A ref is supported by a recorded call of the same server and tool when
// the call's company equals the ref's, each other ref arg is satisfied by
// the call under the rules of snapshotCall.supports (an equal recorded
// argument, an identifier present in the result, a month or date range the
// call's window covers, or a named descriptive arg), and every one of the
// ref's IDs is a record in the result. The first such call, in recording
// order, wins.
//
// It fails closed: if any ref has no supporting snapshot, it returns an
// error wrapping ErrNoSnapshot and leaves findings unchanged.
func (s *Snapshots) Annotate(findings []checks.Finding) error {
	s.mu.Lock()
	calls := slices.Clone(s.calls)
	s.mu.Unlock()

	evidence := make([][]checks.EvidenceRef, len(findings))
	for i, f := range findings {
		refs := slices.Clone(f.Evidence)
		for j, ref := range refs {
			sha, err := matchRef(calls, ref)
			if err != nil {
				return fmt.Errorf("agent: annotate finding %s (%s) evidence %d (%s%s%s): %w",
					f.ID, f.Type, j, ref.Server, toolSep, ref.Tool, err)
			}
			refs[j].Artifact = sha
		}
		evidence[i] = refs
	}
	for i := range findings {
		findings[i].Evidence = evidence[i]
	}
	return nil
}

// matchRef returns the address of the first call that supports ref.
func matchRef(calls []snapshotCall, ref checks.EvidenceRef) (string, error) {
	args, err := refArgs(ref.Args)
	if err != nil {
		return "", err
	}
	// A ref must name its company, so it can never be matched to another
	// company's call; a ref with nothing to match on matches nothing.
	if _, ok := args["company"]; !ok {
		return "", fmt.Errorf("%w: the ref has no company argument", ErrNoSnapshot)
	}
	for _, c := range calls {
		if _, ok := c.args["company"]; !ok {
			continue
		}
		if c.server == ref.Server && c.tool == ref.Tool && c.supports(args, ref.IDs) {
			return c.sha, nil
		}
	}
	return "", ErrNoSnapshot
}

// refArgs decodes an EvidenceRef's args object (exact numbers) into the
// canonical JSON of each value. Empty args are an empty object.
func refArgs(raw json.RawMessage) (map[string]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return map[string]string{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("evidence args are not a JSON object: %w", err)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		b, err := store.Canonical(v)
		if err != nil {
			return nil, fmt.Errorf("evidence arg %s: %w", k, err)
		}
		out[k] = string(b)
	}
	return out, nil
}

// descriptiveArgs names, per tool, the ref args that describe a finding but
// select no evidence: the rule that fired, a service period parsed from an
// item description, and the GST amounts and ITC flag the check computed
// from the records. They are not compared with the call, because no call
// records them; the records they come from must still be named by the
// ref's IDs and be present in the snapshot. The verifier grounds amounts
// in the snapshot's records (verify.go reads the stored snapshot, never
// the ref's args), so no amount here is something the verifier relies on.
//
// The list is fixed and per tool: an arg name not listed here, not
// recorded by the call, not a window arg (month, from, to) and not an
// identifier indexed from the result is unknown, and fails the match.
var descriptiveArgs = map[string]map[string]bool{
	// rules.go (rule, service_period_start) and gstr2b.go's books side.
	toolListPurchaseInvoices: {
		"rule": true, "service_period_start": true,
		"books_tax_paise": true, "igst_paise": true, "cgst_paise": true, "sgst_paise": true,
	},
	// gstr2b.go's 2B side.
	toolListGSTR2BEntries: {
		"gstr2b_tax_paise": true, "igst_paise": true, "cgst_paise": true, "sgst_paise": true,
		"itc_available": true,
	},
}

// supports reports whether the call's snapshot holds the evidence for a
// ref with these canonical args and IDs. Each ref arg is satisfied by the
// first rule that applies to its name, and a rule that applies decides:
//
//  1. The call recorded an argument of that name: the values are equal
//     (company always lands here, so a ref never matches another
//     company's call).
//  2. The result indexes an identifier field of that name (txn_id,
//     voucher_no, account, ...): the value is present in the result.
//  3. month (YYYY-MM): the call's period equals it, or the call's
//     from_date..to_date window covers the whole month.
//  4. from and to (YYYY-MM-DD, both required): the call's from_date..to_date
//     window covers from..to.
//  5. The name is in descriptiveArgs for the call's tool: ignored.
//
// Anything else is unknown and fails. Every ID must be a record in the
// result.
func (c snapshotCall) supports(args map[string]string, ids []string) bool {
	for k, v := range args {
		if cv, ok := c.args[k]; ok {
			if cv != v {
				return false
			}
			continue
		}
		if vals, ok := c.keys[k]; ok {
			s, ok := canonString(v)
			if !ok || !vals[s] {
				return false
			}
			continue
		}
		switch {
		case k == "month":
			if !c.coversMonth(v) {
				return false
			}
		case k == "from" || k == "to":
			from, okF := args["from"]
			to, okT := args["to"]
			if !okF || !okT || !c.coversRange(from, to) {
				return false
			}
		case descriptiveArgs[c.tool][k]:
		default:
			return false
		}
	}
	for _, id := range ids {
		if !c.ids[id] {
			return false
		}
	}
	return true
}

// canonString decodes a canonical JSON string value.
func canonString(v string) (string, bool) {
	var s string
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		return "", false
	}
	return s, true
}

// canonDate decodes a canonical JSON YYYY-MM-DD string.
func canonDate(v string) (time.Time, bool) {
	s, ok := canonString(v)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(wireDate, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// window is the call's recorded from_date..to_date, when it has both.
func (c snapshotCall) window() (time.Time, time.Time, bool) {
	fv, okF := c.args["from_date"]
	tv, okT := c.args["to_date"]
	if !okF || !okT {
		return time.Time{}, time.Time{}, false
	}
	from, okF := canonDate(fv)
	to, okT := canonDate(tv)
	if !okF || !okT || to.Before(from) {
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// coversMonth reports whether the call holds the whole month named by the
// canonical JSON value v: a period-scoped call (list_gstr2b_entries) for
// that period, or a date-range call whose window contains the month.
func (c snapshotCall) coversMonth(v string) bool {
	if p, ok := c.args["period"]; ok {
		return p == v
	}
	m, ok := canonString(v)
	if !ok {
		return false
	}
	first, last, err := monthRange(m)
	if err != nil {
		return false
	}
	from, to, ok := c.window()
	return ok && !first.Before(from) && !last.After(to)
}

// coversRange reports whether the call's window contains the ref's
// from..to (canonical JSON dates).
func (c snapshotCall) coversRange(fromV, toV string) bool {
	rf, okF := canonDate(fromV)
	rt, okT := canonDate(toV)
	if !okF || !okT || rt.Before(rf) {
		return false
	}
	from, to, ok := c.window()
	return ok && !rf.Before(from) && !rt.After(to)
}

// resultIndex holds the identifiers found in one tool result.
type resultIndex struct {
	keys map[string]map[string]bool
	ids  map[string]bool
}

func newIndex() resultIndex {
	return resultIndex{keys: map[string]map[string]bool{}, ids: map[string]bool{}}
}

// add records value under the identifier field key; asID also makes it a
// record identifier for EvidenceRef.IDs. Empty values are skipped.
func (x resultIndex) add(key, value string, asID bool) {
	if value == "" {
		return
	}
	if x.keys[key] == nil {
		x.keys[key] = map[string]bool{}
	}
	x.keys[key][value] = true
	if asID {
		x.ids[value] = true
	}
}

// argDate formats a date argument as the MCP tools take it; the zero time
// is "".
func argDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(wireDate)
}

func rangeArgMap(company string, from, to time.Time) map[string]any {
	return map[string]any{"company": company, "from_date": argDate(from), "to_date": argDate(to)}
}

// ---- books ----

type snapshotBooks struct {
	inner checks.BooksReader
	snap  *Snapshots
}

// SnapshotBooks wraps a BooksReader so every result is stored as a
// tool_result artifact recorded in snap.
func SnapshotBooks(inner checks.BooksReader, snap *Snapshots) checks.BooksReader {
	return &snapshotBooks{inner: inner, snap: snap}
}

func (b *snapshotBooks) TrialBalance(ctx context.Context, company string, from, to time.Time) (ledger.TB, error) {
	tb, err := b.inner.TrialBalance(ctx, company, from, to)
	if err != nil {
		return ledger.TB{}, err
	}
	idx := newIndex()
	for _, r := range tb.Rows {
		idx.add("account", r.Account, true)
	}
	if err := b.snap.record(ctx, ServerBooks, toolGetTrialBalance, rangeArgMap(company, from, to), tb, idx); err != nil {
		return ledger.TB{}, err
	}
	return tb, nil
}

func (b *snapshotBooks) GLEntries(ctx context.Context, company string, from, to time.Time) ([]ledger.GLEntry, error) {
	es, err := b.inner.GLEntries(ctx, company, from, to)
	if err != nil {
		return nil, err
	}
	idx := newIndex()
	for _, e := range es {
		idx.add("name", e.Name, true)
		idx.add("gl_entry", e.Name, false)
		idx.add("voucher_no", e.VoucherNo, true)
		idx.add("account", e.Account, false)
		idx.add("party", e.Party, false)
	}
	if err := b.snap.record(ctx, ServerBooks, toolListGLEntries, rangeArgMap(company, from, to), es, idx); err != nil {
		return nil, err
	}
	return es, nil
}

func (b *snapshotBooks) PurchaseInvoices(ctx context.Context, company string, from, to time.Time) ([]ledger.PurchaseInvoice, error) {
	pis, err := b.inner.PurchaseInvoices(ctx, company, from, to)
	if err != nil {
		return nil, err
	}
	idx := newIndex()
	for _, p := range pis {
		idx.add("name", p.Name, true)
		idx.add("invoice", p.Name, false)
		idx.add("supplier", p.Supplier, false)
		idx.add("bill_no", p.BillNo, false)
		// The item expense accounts: rules.go names the account a policy
		// rule fired on.
		for _, it := range p.Items {
			idx.add("account", it.ExpenseAccount, false)
		}
	}
	if err := b.snap.record(ctx, ServerBooks, toolListPurchaseInvoices, rangeArgMap(company, from, to), pis, idx); err != nil {
		return nil, err
	}
	return pis, nil
}

func (b *snapshotBooks) SalesInvoices(ctx context.Context, company string, from, to time.Time) ([]ledger.SalesInvoice, error) {
	sis, err := b.inner.SalesInvoices(ctx, company, from, to)
	if err != nil {
		return nil, err
	}
	idx := newIndex()
	for _, s := range sis {
		idx.add("name", s.Name, true)
		idx.add("invoice", s.Name, false)
		idx.add("customer", s.Customer, false)
	}
	if err := b.snap.record(ctx, ServerBooks, toolListSalesInvoices, rangeArgMap(company, from, to), sis, idx); err != nil {
		return nil, err
	}
	return sis, nil
}

func (b *snapshotBooks) PaymentEntries(ctx context.Context, company string, from, to time.Time) ([]ledger.PaymentEntry, error) {
	ps, err := b.inner.PaymentEntries(ctx, company, from, to)
	if err != nil {
		return nil, err
	}
	idx := newIndex()
	for _, p := range ps {
		idx.add("name", p.Name, true)
		idx.add("payment", p.Name, false)
		idx.add("party", p.Party, false)
		idx.add("reference_no", p.ReferenceNo, false)
	}
	if err := b.snap.record(ctx, ServerBooks, toolListPayments, rangeArgMap(company, from, to), ps, idx); err != nil {
		return nil, err
	}
	return ps, nil
}

func (b *snapshotBooks) AccountHistory(ctx context.Context, company, account string, through string, months int) ([]ledger.MonthTotal, error) {
	hs, err := b.inner.AccountHistory(ctx, company, account, through, months)
	if err != nil {
		return nil, err
	}
	idx := newIndex()
	for _, h := range hs {
		idx.add("month", h.Month, true)
	}
	args := map[string]any{"company": company, "account": account, "months": months, "through_month": through}
	if err := b.snap.record(ctx, ServerBooks, toolGetAccountHistory, args, hs, idx); err != nil {
		return nil, err
	}
	return hs, nil
}

func (b *snapshotBooks) RecurringSuppliers(ctx context.Context, company string, beforeMonth string, lookbackMonths int, minOccurrences int, amountBandPct int) ([]checks.RecurringSupplier, error) {
	rs, err := b.inner.RecurringSuppliers(ctx, company, beforeMonth, lookbackMonths, minOccurrences, amountBandPct)
	if err != nil {
		return nil, err
	}
	idx := newIndex()
	for _, r := range rs {
		idx.add("supplier", r.Supplier, true)
	}
	args := map[string]any{
		"company": company, "before_month": beforeMonth, "lookback_months": lookbackMonths,
		"min_occurrences": minOccurrences, "amount_band_pct": amountBandPct,
	}
	if err := b.snap.record(ctx, ServerBooks, toolListRecurringSuppliers, args, rs, idx); err != nil {
		return nil, err
	}
	return rs, nil
}

// ---- evidence ----

type snapshotEvidence struct {
	inner checks.EvidenceReader
	snap  *Snapshots
}

// SnapshotEvidence wraps an EvidenceReader so every result is stored as a
// tool_result artifact recorded in snap.
func SnapshotEvidence(inner checks.EvidenceReader, snap *Snapshots) checks.EvidenceReader {
	return &snapshotEvidence{inner: inner, snap: snap}
}

func (e *snapshotEvidence) BankLines(ctx context.Context, company string, from, to time.Time) ([]store.BankLine, error) {
	ls, err := e.inner.BankLines(ctx, company, from, to)
	if err != nil {
		return nil, err
	}
	idx := newIndex()
	for _, l := range ls {
		idx.add("txn_id", l.TxnID, true)
		idx.add("bank_txn_id", l.TxnID, false)
	}
	if err := e.snap.record(ctx, ServerEvidence, toolListBankLines, rangeArgMap(company, from, to), ls, idx); err != nil {
		return nil, err
	}
	return ls, nil
}

func (e *snapshotEvidence) GSTR2BEntries(ctx context.Context, company string, period string) ([]store.GSTR2BEntry, error) {
	gs, err := e.inner.GSTR2BEntries(ctx, company, period)
	if err != nil {
		return nil, err
	}
	idx := newIndex()
	for _, g := range gs {
		idx.add("invoice_no", g.InvoiceNo, true)
		idx.add("invoice_no_norm", g.InvoiceNoNorm, true)
		idx.add("supplier_gstin", g.SupplierGSTIN, false)
	}
	args := map[string]any{"company": company, "period": period}
	if err := e.snap.record(ctx, ServerEvidence, toolListGSTR2BEntries, args, gs, idx); err != nil {
		return nil, err
	}
	return gs, nil
}
