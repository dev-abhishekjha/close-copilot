package seed

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
)

// The DocTypes the book writer posts.
const (
	DocSalesInvoice    = "Sales Invoice"
	DocPurchaseInvoice = "Purchase Invoice"
	DocPaymentEntry    = "Payment Entry"
	DocJournalEntry    = "Journal Entry"
)

// gatewayReceiptDocType is the voucher a gateway_receipt posts as. The spec
// allowed a Journal Entry in case ERPNext refused a Payment Entry whose
// paid_to is not a Bank or Cash account. ERPNext 15 only filters paid_to to
// Bank and Cash accounts in the form (payment_entry.js); the server accepts
// a Receive into Payment Gateway Clearing, a plain Asset account, and
// TestIntegrationBooksResume posts every gateway receipt that way. So a
// gateway_receipt is always a Payment Entry (Receive) into the clearing
// account, like a direct receipt into the bank.
const gatewayReceiptDocType = DocPaymentEntry

// BookDocType returns the DocType an event kind is posted as, or "" for a
// kind the books don't record (gateway_settlement, left for the agent).
func BookDocType(kind string) string {
	switch kind {
	case EventSale:
		return DocSalesInvoice
	case EventPurchase:
		return DocPurchaseInvoice
	case EventReceipt, EventVendorPayment:
		return DocPaymentEntry
	case EventGatewayReceipt:
		return gatewayReceiptDocType
	case EventPayroll, EventBankCharge, EventInterest, EventPrepaidAmortisation:
		return DocJournalEntry
	}
	return ""
}

// bookStages are the DocTypes posted together: stage 2 links to stage 1's
// invoices. Within a stage, each DocType has one worker.
var bookStages = [][]string{
	{DocSalesInvoice, DocPurchaseInvoice},
	{DocPaymentEntry, DocJournalEntry},
}

// ErrStopped is returned (wrapped) by WriteBooks when BooksOptions.StopAfter
// stopped it before every event was posted. The result is still filled in;
// its Map is incomplete, so don't write it.
var ErrStopped = errors.New("stopped after the requested number of new documents")

// defaultBookWorkers is BooksOptions.Workers when it is zero.
const defaultBookWorkers = 4

// BooksOptions tune WriteBooks.
type BooksOptions struct {
	// Suppliers is the profile's supplier list: a purchase's item comes
	// from its supplier's kind (ItemFor). Required when the world has
	// purchases.
	Suppliers []Supplier
	// Prior maps ExtIDs posted in earlier months, for Refs that point
	// outside this world (Phase 2's chained months).
	Prior ERPMap
	// StopAfter stops cleanly after inserting this many new documents (0 =
	// no limit), so tests can simulate a crash. WriteBooks then returns
	// ErrStopped.
	StopAfter int
	// Workers bounds how many DocTypes post at once (default 4). Each
	// DocType is always posted by one worker, in (Date, ExtID) order.
	Workers int
	// Log receives progress; nil discards it.
	Log *slog.Logger
}

// BookCounts are one DocType's results.
type BookCounts struct {
	// Created documents were inserted and submitted by this run.
	Created int `json:"created"`
	// SubmittedFromDraft were drafts carrying one of the world's ExtIDs (a
	// crash between insert and submit) that this run submitted.
	SubmittedFromDraft int `json:"submitted_from_draft"`
	// AlreadyPresent were submitted before this run and left alone.
	AlreadyPresent int `json:"already_present"`
}

// BooksResult is what WriteBooks did.
type BooksResult struct {
	Company string `json:"company"`
	Month   string `json:"month"`
	// Counts is keyed by DocType and has an entry for every DocType the
	// world posts to.
	Counts map[string]*BookCounts `json:"counts"`
	// Skipped counts the events the books don't record, by event kind
	// (gateway_settlement).
	Skipped map[string]int `json:"skipped"`
	// Stopped is true when StopAfter ended the run early.
	Stopped bool `json:"stopped"`
	// Map holds every event posted (or found posted) by this run. It is
	// complete only when WriteBooks returns no error.
	Map ERPMap `json:"-"`
}

// Changes is the number of documents created or submitted.
func (r BooksResult) Changes() int {
	n := 0
	for _, c := range r.Counts {
		n += c.Created + c.SubmittedFromDraft
	}
	return n
}

// existingDoc is a document already in ERPNext that carries an ExtID.
type existingDoc struct {
	DocType   string
	Name      string
	DocStatus int
}

// WriteBooks posts every book-side event of w to ERPNext as a submitted
// document with copilot_ext_id = ExtID, and returns the name each ExtID got.
// rep is Bootstrap's report for the world's company; Bootstrap must have
// run first, so every account, party and template name comes from it.
//
// Each DocType is posted by one worker, in (Date, ExtID) order, so two
// runs from a clean site give identical names: Sales and Purchase
// Invoices first (stage 1), then Payment and Journal Entries (stage 2),
// which link to stage 1's invoices. A failed document stops its worker;
// the others finish their current document and stop, and the error lists
// every failure.
//
// WriteBooks resumes: it first lists the documents of the month that
// carry an ExtID. A submitted one is recorded and skipped, a draft (a
// crash between insert and submit) is submitted, and a cancelled one is an
// error. A rerun after a crash therefore finishes the job without
// duplicates, and a rerun after success changes nothing.
func WriteBooks(ctx context.Context, c *frappe.Client, rep BootstrapReport, w World, opt BooksOptions) (BooksResult, error) {
	res := BooksResult{
		Company: rep.Company,
		Month:   w.Month,
		Counts:  map[string]*BookCounts{},
		Skipped: map[string]int{},
		Map:     ERPMap{},
	}
	if c == nil {
		return res, errors.New("write books: nil frappe client")
	}
	bw, err := newBookWriter(c, rep, w, opt, &res)
	if err != nil {
		return res, fmt.Errorf("write books %s %s: %w", w.Company, w.Month, err)
	}
	if err := bw.loadExisting(ctx); err != nil {
		return res, fmt.Errorf("write books %s %s: %w", w.Company, w.Month, err)
	}
	for _, stage := range bookStages {
		if err := bw.runStage(ctx, stage); err != nil {
			return res, fmt.Errorf("write books %s %s: %w", w.Company, w.Month, err)
		}
		if bw.stopped.Load() {
			res.Stopped = true
			return res, fmt.Errorf("write books %s %s: %w (%d)", w.Company, w.Month, ErrStopped, opt.StopAfter)
		}
	}
	return res, nil
}

type bookWriter struct {
	c        *frappe.Client
	rep      BootstrapReport
	opt      BooksOptions
	log      *slog.Logger
	start    time.Time         // first day of the month
	end      time.Time         // last day of the month
	kinds    map[string]string // supplier id -> kind
	events   map[string]Event  // ExtID -> book-side event
	queues   map[string][]Event
	existing map[string]existingDoc // ExtID -> document found before the run

	mu  sync.Mutex
	res *BooksResult

	inserted atomic.Int64
	stopped  atomic.Bool
	failed   atomic.Bool
}

func newBookWriter(c *frappe.Client, rep BootstrapReport, w World, opt BooksOptions, res *BooksResult) (*bookWriter, error) {
	start, err := ParseMonth(w.Month)
	if err != nil {
		return nil, err
	}
	if rep.Company == "" {
		return nil, errors.New("the bootstrap report has no company; run Bootstrap first")
	}
	if rep.ExtIDField != ExtIDField {
		return nil, fmt.Errorf("the bootstrap report's external-ID field is %q, want %q; run Bootstrap first", rep.ExtIDField, ExtIDField)
	}
	if opt.Workers < 0 || opt.StopAfter < 0 {
		return nil, fmt.Errorf("workers %d and stop-after %d must not be negative", opt.Workers, opt.StopAfter)
	}
	if opt.Workers == 0 {
		opt.Workers = defaultBookWorkers
	}
	log := opt.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	bw := &bookWriter{
		c:        c,
		rep:      rep,
		opt:      opt,
		log:      log,
		start:    start,
		end:      start.AddDate(0, 1, -1),
		kinds:    map[string]string{},
		events:   map[string]Event{},
		queues:   map[string][]Event{},
		existing: map[string]existingDoc{},
		res:      res,
	}
	for _, s := range opt.Suppliers {
		bw.kinds[s.ID] = s.Kind
	}

	var errs []error
	seen := map[string]bool{}
	for _, ev := range w.Events {
		if ev.ExtID == "" {
			errs = append(errs, fmt.Errorf("a %s event on %s has no ExtID", ev.Kind, ev.Date))
			continue
		}
		if seen[ev.ExtID] {
			errs = append(errs, fmt.Errorf("ExtID %s appears twice in the world", ev.ExtID))
			continue
		}
		seen[ev.ExtID] = true
		if !slices.Contains(EventKinds, ev.Kind) {
			errs = append(errs, fmt.Errorf("%s: unknown event kind %q", ev.ExtID, ev.Kind))
			continue
		}
		dt := BookDocType(ev.Kind)
		if dt == "" {
			res.Skipped[ev.Kind]++
			continue
		}
		d, err := time.Parse(dateLayout, ev.Date)
		if err != nil || d.Before(bw.start) || d.After(bw.end) {
			errs = append(errs, fmt.Errorf("%s: date %q is not in %s", ev.ExtID, ev.Date, w.Month))
			continue
		}
		bw.events[ev.ExtID] = ev
		bw.queues[dt] = append(bw.queues[dt], ev)
		if res.Counts[dt] == nil {
			res.Counts[dt] = &BookCounts{}
		}
	}
	for _, q := range bw.queues {
		slices.SortStableFunc(q, func(a, b Event) int {
			return cmp.Or(cmp.Compare(a.Date, b.Date), cmp.Compare(a.ExtID, b.ExtID))
		})
	}
	// Map every event before any I/O, with placeholder names for the
	// invoices stage 2 links to, so a mapping problem (a missing account,
	// supplier or reference) fails the run before it posts anything.
	for _, ev := range w.Events {
		if _, ok := bw.events[ev.ExtID]; !ok {
			continue
		}
		if _, err := bw.build(ev, bw.planRef); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return bw, nil
}

// refFunc resolves a referenced ExtID to the ERPNext name of a document of
// the wanted DocType.
type refFunc func(ext, doctype string) (string, error)

// planRef resolves a reference before anything is posted: the ExtID must
// be one of the world's events of the wanted DocType, or in Prior.
func (bw *bookWriter) planRef(ext, doctype string) (string, error) {
	if ev, ok := bw.events[ext]; ok {
		if got := BookDocType(ev.Kind); got != doctype {
			return "", fmt.Errorf("reference %s is a %s, want a %s", ext, got, doctype)
		}
		return "<" + ext + ">", nil
	}
	return bw.priorRef(ext, doctype)
}

func (bw *bookWriter) priorRef(ext, doctype string) (string, error) {
	ref, ok := bw.opt.Prior[ext]
	if !ok {
		return "", fmt.Errorf("reference %s is neither in this world nor in the prior months' maps", ext)
	}
	if ref.DocType != doctype {
		return "", fmt.Errorf("reference %s is %s %s, want a %s", ext, ref.DocType, ref.Name, doctype)
	}
	return ref.Name, nil
}

// postedRef resolves a reference while posting: this run's map first
// (stage 1 has finished), then Prior.
func (bw *bookWriter) postedRef(ext, doctype string) (string, error) {
	bw.mu.Lock()
	ref, ok := bw.res.Map[ext]
	bw.mu.Unlock()
	if ok {
		if ref.DocType != doctype {
			return "", fmt.Errorf("reference %s is %s %s, want a %s", ext, ref.DocType, ref.Name, doctype)
		}
		return ref.Name, nil
	}
	if _, inWorld := bw.events[ext]; inWorld {
		return "", fmt.Errorf("reference %s has not been posted", ext)
	}
	return bw.priorRef(ext, doctype)
}

// loadExisting lists, per DocType, the month's documents of the company
// that carry an ExtID (drafts, submitted and cancelled).
func (bw *bookWriter) loadExisting(ctx context.Context) error {
	var errs []error
	for _, stage := range bookStages {
		for _, dt := range stage {
			rows, err := frappe.List[doc](ctx, bw.c, dt, frappe.Query{
				Fields: []string{"name", ExtIDField, "docstatus"},
				Filters: [][]any{
					{"company", "=", bw.rep.Company},
					{"posting_date", "between", []string{bw.start.Format(dateLayout), bw.end.Format(dateLayout)}},
					{ExtIDField, "is", "set"},
					{"docstatus", "in", []int{0, 1, 2}},
				},
				OrderBy: "name asc",
			})
			if err != nil {
				return fmt.Errorf("list existing %s: %w", dt, err)
			}
			for _, r := range rows {
				ext, name := str(r[ExtIDField]), str(r["name"])
				ev, ours := bw.events[ext]
				if !ours {
					continue // another world's document, or one this world doesn't post
				}
				if prev, dup := bw.existing[ext]; dup {
					errs = append(errs, fmt.Errorf("ExtID %s is on both %s %s and %s %s", ext, prev.DocType, prev.Name, dt, name))
					continue
				}
				status := str(r["docstatus"])
				ed := existingDoc{DocType: dt, Name: name}
				switch status {
				case "0":
					ed.DocStatus = 0
				case "1":
					ed.DocStatus = 1
				case "2":
					errs = append(errs, fmt.Errorf("%s %s (%s) is cancelled; reset the site or delete it before seeding", dt, name, ext))
					continue
				default:
					errs = append(errs, fmt.Errorf("%s %s (%s) has docstatus %q", dt, name, ext, status))
					continue
				}
				if want := BookDocType(ev.Kind); want != dt {
					errs = append(errs, fmt.Errorf("%s %s carries ExtID %s, a %s event that posts as a %s", dt, name, ext, ev.Kind, want))
					continue
				}
				bw.existing[ext] = ed
			}
		}
	}
	return errors.Join(errs...)
}

// runStage posts the stage's DocTypes, one worker each, at most Workers at
// a time.
func (bw *bookWriter) runStage(ctx context.Context, doctypes []string) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	sem := make(chan struct{}, bw.opt.Workers)
	for _, dt := range doctypes {
		q := bw.queues[dt]
		if len(q) == 0 {
			continue
		}
		// Take the slot before starting the worker, so with fewer workers
		// than DocTypes they start in stage order.
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := bw.work(ctx, dt, q); err != nil {
				bw.failed.Store(true)
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	slices.SortFunc(errs, func(a, b error) int { return cmp.Compare(a.Error(), b.Error()) })
	return errors.Join(errs...)
}

// work posts one DocType's events in order. It stops at its first failure,
// or before its next document once another worker failed or StopAfter is
// reached.
func (bw *bookWriter) work(ctx context.Context, dt string, q []Event) error {
	for _, ev := range q {
		if bw.failed.Load() || bw.stopped.Load() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: %w", dt, err)
		}
		if err := bw.post(ctx, dt, ev); err != nil {
			return err
		}
	}
	return nil
}

func (bw *bookWriter) record(dt string, ev Event, name string, count func(*BookCounts)) {
	bw.mu.Lock()
	defer bw.mu.Unlock()
	bw.res.Map[ev.ExtID] = ERPRef{DocType: dt, Name: name}
	count(bw.res.Counts[dt])
}

func (bw *bookWriter) post(ctx context.Context, dt string, ev Event) error {
	if ed, ok := bw.existing[ev.ExtID]; ok {
		if ed.DocStatus == 1 {
			bw.record(dt, ev, ed.Name, func(c *BookCounts) { c.AlreadyPresent++ })
			return nil
		}
		if err := bw.submit(ctx, dt, ev, ed.Name); err != nil {
			return err
		}
		bw.record(dt, ev, ed.Name, func(c *BookCounts) { c.SubmittedFromDraft++ })
		bw.log.Info("submitted draft", "doctype", dt, "name", ed.Name, "ext_id", ev.ExtID)
		return nil
	}
	if n := bw.opt.StopAfter; n > 0 && bw.inserted.Add(1) > int64(n) {
		bw.stopped.Store(true)
		return nil
	}
	body, err := bw.build(ev, bw.postedRef)
	if err != nil {
		return err
	}
	saved, err := frappe.Insert(ctx, bw.c, dt, body)
	if err != nil {
		return fmt.Errorf("insert %s for %s: %w", dt, ev.ExtID, err)
	}
	name := str(saved["name"])
	if name == "" {
		return fmt.Errorf("insert %s for %s: no name in the response", dt, ev.ExtID)
	}
	if err := bw.submit(ctx, dt, ev, name); err != nil {
		return err
	}
	bw.record(dt, ev, name, func(c *BookCounts) { c.Created++ })
	bw.log.Info("posted", "doctype", dt, "name", name, "ext_id", ev.ExtID, "kind", ev.Kind)
	return nil
}

// totalField is the field of a submitted document that must equal the
// event's Gross.
var totalField = map[string]string{
	DocSalesInvoice:    "grand_total",
	DocPurchaseInvoice: "grand_total",
	DocPaymentEntry:    "paid_amount",
	DocJournalEntry:    "total_debit",
}

// submit submits a draft and checks that ERPNext kept the event's ExtID and
// amount.
func (bw *bookWriter) submit(ctx context.Context, dt string, ev Event, name string) error {
	out, err := frappe.Submit(ctx, bw.c, dt, name)
	if err != nil {
		return fmt.Errorf("submit %s %s (%s): %w", dt, name, ev.ExtID, err)
	}
	if s := str(out["docstatus"]); s != "1" {
		return fmt.Errorf("submit %s %s (%s): docstatus is %q after submit", dt, name, ev.ExtID, s)
	}
	if got := str(out[ExtIDField]); got != ev.ExtID {
		return fmt.Errorf("submit %s %s: %s is %q, want %q", dt, name, ExtIDField, got, ev.ExtID)
	}
	field := totalField[dt]
	got, err := paiseField(out[field])
	if err != nil {
		return fmt.Errorf("submit %s %s (%s): %s: %w", dt, name, ev.ExtID, field, err)
	}
	if got != ev.Gross {
		return fmt.Errorf("submit %s %s (%s): %s is %s, the event's gross is %s", dt, name, ev.ExtID, field, got.Rupees(), ev.Gross.Rupees())
	}
	return nil
}

// paiseField reads an amount from a decoded document: a json.Number or a
// decimal string.
func paiseField(v any) (money.Paise, error) {
	switch v := v.(type) {
	case json.Number:
		return money.FromJSONNumber(v)
	case string:
		return money.ParseRupees(v)
	case nil:
		return 0, errors.New("missing")
	}
	return 0, fmt.Errorf("unexpected %T", v)
}

// amount is a paise amount as ERPNext takes it: rupees with two decimals.
func amount(p money.Paise) json.Number { return json.Number(p.Rupees()) }

// build maps an event to the body of its document.
func (bw *bookWriter) build(ev Event, ref refFunc) (doc, error) {
	var (
		d   doc
		err error
	)
	switch ev.Kind {
	case EventSale:
		d, err = bw.salesInvoice(ev)
	case EventPurchase:
		d, err = bw.purchaseInvoice(ev)
	case EventReceipt:
		d, err = bw.receipt(ev, ref, bw.rep.BankAccount)
	case EventGatewayReceipt:
		d, err = bw.receipt(ev, ref, bw.rep.Accounts[AccountPaymentGatewayClearing])
	case EventVendorPayment:
		d, err = bw.vendorPayment(ev, ref)
	case EventPayroll, EventBankCharge, EventInterest, EventPrepaidAmortisation:
		d, err = bw.journal(ev)
	default:
		err = fmt.Errorf("kind %q has no document", ev.Kind)
	}
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", ev.ExtID, ev.Kind, err)
	}
	return d, nil
}

// common are the fields every voucher carries.
func (bw *bookWriter) common(ev Event) doc {
	return doc{
		"company":          bw.rep.Company,
		"posting_date":     ev.Date,
		"set_posting_time": 1,
		ExtIDField:         ev.ExtID,
	}
}

// account resolves a base account name through the bootstrap report.
func (bw *bookWriter) account(base string) (string, error) {
	name := bw.rep.Accounts[base]
	if name == "" {
		return "", fmt.Errorf("account %q is not in the bootstrap report", base)
	}
	return name, nil
}

func (bw *bookWriter) accounts(bases ...string) ([]string, error) {
	out := make([]string, len(bases))
	for i, b := range bases {
		n, err := bw.account(b)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

// taxRows are the explicit (Actual) GST rows of an invoice, in IGST, CGST,
// SGST order; purchase rows carry the Purchase Taxes and Charges fields.
func taxRows(ev Event, accs GSTAccounts, purchase bool) ([]doc, error) {
	var rows []doc
	for _, t := range []struct {
		label string
		head  string
		amt   money.Paise
	}{
		{"IGST", accs.IGST, ev.IGST},
		{"CGST", accs.CGST, ev.CGST},
		{"SGST", accs.SGST, ev.SGST},
	} {
		if t.amt == 0 {
			continue
		}
		if t.amt < 0 {
			return nil, fmt.Errorf("negative %s %s", t.label, t.amt.Rupees())
		}
		if t.head == "" {
			return nil, fmt.Errorf("no %s account in the bootstrap report", t.label)
		}
		r := doc{
			"charge_type":  "Actual",
			"account_head": t.head,
			"tax_amount":   amount(t.amt),
			"description":  t.label,
		}
		if purchase {
			r["category"] = "Total"
			r["add_deduct_tax"] = "Add"
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// checkGross checks that an invoice's gross is its taxable amount plus GST.
func checkGross(ev Event) error {
	if ev.Taxable <= 0 {
		return fmt.Errorf("taxable %s is not positive", ev.Taxable.Rupees())
	}
	if sum := ev.Taxable + ev.IGST + ev.CGST + ev.SGST; sum != ev.Gross {
		return fmt.Errorf("gross %s is not taxable plus GST (%s)", ev.Gross.Rupees(), sum.Rupees())
	}
	return nil
}

// itemTaxTemplate returns the template for the event's GST rate, or "" when
// the event carries no GST.
func (bw *bookWriter) itemTaxTemplate(ev Event) (string, error) {
	if ev.IGST+ev.CGST+ev.SGST == 0 {
		return "", nil
	}
	if ev.Meta.GSTRate <= 0 {
		return "", fmt.Errorf("GST of %s without a GST rate", (ev.IGST + ev.CGST + ev.SGST).Rupees())
	}
	key := fmt.Sprint(ev.Meta.GSTRate)
	t := bw.rep.ItemTaxTemplates[key]
	if t == "" {
		return "", fmt.Errorf("no Item Tax Template for GST %s%% in the bootstrap report", key)
	}
	return t, nil
}

// salesInvoice: Dr Debtors, Cr Sales and output GST.
func (bw *bookWriter) salesInvoice(ev Event) (doc, error) {
	if ev.Party == "" {
		return nil, errors.New("no customer")
	}
	if err := checkGross(ev); err != nil {
		return nil, err
	}
	accs, err := bw.accounts(AccountDebtors, ev.Account)
	if err != nil {
		return nil, err
	}
	tmpl, err := bw.itemTaxTemplate(ev)
	if err != nil {
		return nil, err
	}
	taxes, err := taxRows(ev, bw.rep.OutputGST, false)
	if err != nil {
		return nil, err
	}
	date, _ := time.Parse(dateLayout, ev.Date)
	item := doc{
		"item_code":      ItemFor(EventSale, "", false),
		"qty":            1,
		"rate":           amount(ev.Taxable),
		"income_account": accs[1],
	}
	if ev.Meta.Description != "" {
		item["description"] = ev.Meta.Description
	}
	if tmpl != "" {
		item["item_tax_template"] = tmpl
	}
	d := bw.common(ev)
	d["customer"] = ev.Party
	d["due_date"] = date.AddDate(0, 0, 15).Format(dateLayout)
	d["debit_to"] = accs[0]
	d["update_stock"] = 0
	d["disable_rounded_total"] = 1
	d["remarks"] = ev.Narration
	if bw.rep.CompanyAddress != "" {
		d["company_address"] = bw.rep.CompanyAddress
	}
	d["items"] = []doc{item}
	d["taxes"] = orEmpty(taxes)
	return d, nil
}

// purchaseDescription is the item description: the event's description and,
// when the event has a service period its description doesn't already
// state, that period.
func purchaseDescription(ev Event) string {
	desc := ev.Meta.Description
	if ev.Meta.ServiceFrom != "" && !strings.Contains(desc, "Service period") {
		desc = strings.TrimSpace(fmt.Sprintf("%s Service period: %s to %s", desc, ev.Meta.ServiceFrom, ev.Meta.ServiceTo))
	}
	return desc
}

// purchaseInvoice: Dr Account and input GST, Cr Creditors.
func (bw *bookWriter) purchaseInvoice(ev Event) (doc, error) {
	sid := ev.Meta.SupplierID
	supplier := bw.rep.Suppliers[sid]
	if supplier == "" {
		return nil, fmt.Errorf("supplier %q is not in the bootstrap report", sid)
	}
	kind, ok := bw.kinds[sid]
	if !ok {
		return nil, fmt.Errorf("supplier %q is not in BooksOptions.Suppliers", sid)
	}
	if ev.InvoiceNo == "" {
		return nil, errors.New("no invoice number")
	}
	if err := checkGross(ev); err != nil {
		return nil, err
	}
	accs, err := bw.accounts(AccountCreditors, ev.Account)
	if err != nil {
		return nil, err
	}
	registered := ev.Meta.SupplierGSTIN != ""
	if !registered && ev.IGST+ev.CGST+ev.SGST != 0 {
		return nil, errors.New("GST on a bill from an unregistered supplier")
	}
	tmpl, err := bw.itemTaxTemplate(ev)
	if err != nil {
		return nil, err
	}
	taxes, err := taxRows(ev, bw.rep.InputGST, true)
	if err != nil {
		return nil, err
	}
	itemCode := ItemFor(EventPurchase, kind, ev.Account == AccountOfficeEquipment)
	if itemCode == "" {
		return nil, fmt.Errorf("no item for supplier kind %q", kind)
	}
	item := doc{
		"item_code":       itemCode,
		"qty":             1,
		"rate":            amount(ev.Taxable),
		"expense_account": accs[1],
		"description":     purchaseDescription(ev),
	}
	if tmpl != "" {
		item["item_tax_template"] = tmpl
	}
	d := bw.common(ev)
	d["supplier"] = supplier
	d["bill_no"] = ev.InvoiceNo
	d["bill_date"] = ev.Date
	d["credit_to"] = accs[0]
	d["update_stock"] = 0
	d["disable_rounded_total"] = 1
	d["remarks"] = ev.Narration
	if addr := bw.rep.SupplierAddresses[sid]; registered && addr != "" {
		d["supplier_address"] = addr
	}
	if bw.rep.CompanyAddress != "" {
		d["billing_address"] = bw.rep.CompanyAddress
	}
	d["items"] = []doc{item}
	d["taxes"] = orEmpty(taxes)
	return d, nil
}

func orEmpty(rows []doc) []doc {
	if rows == nil {
		return []doc{}
	}
	return rows
}

// reference is a payment's single invoice reference.
func reference(ev Event, ref refFunc, doctype string) (string, error) {
	if len(ev.Refs) != 1 {
		return "", fmt.Errorf("%d refs, want 1", len(ev.Refs))
	}
	return ref(ev.Refs[0], doctype)
}

// bankRef is the bank reference a voucher carries: the event's BankRef, or
// its ExtID when there is none.
func bankRef(ev Event) string {
	if ev.BankRef != "" {
		return ev.BankRef
	}
	return ev.ExtID
}

func positiveGross(ev Event) error {
	if ev.Gross <= 0 {
		return fmt.Errorf("gross %s is not positive", ev.Gross.Rupees())
	}
	return nil
}

// receipt: a Payment Entry (Receive) from the customer into paidTo (the
// bank, or Payment Gateway Clearing for a gateway receipt), settling the
// sale in Refs[0].
func (bw *bookWriter) receipt(ev Event, ref refFunc, paidTo string) (doc, error) {
	if ev.Party == "" {
		return nil, errors.New("no customer")
	}
	if err := positiveGross(ev); err != nil {
		return nil, err
	}
	if paidTo == "" {
		return nil, errors.New("no receiving account in the bootstrap report")
	}
	debtors, err := bw.account(AccountDebtors)
	if err != nil {
		return nil, err
	}
	inv, err := reference(ev, ref, DocSalesInvoice)
	if err != nil {
		return nil, err
	}
	d := bw.payment(ev, "Receive", PartyCustomer, debtors, paidTo)
	d["references"] = []doc{{
		"reference_doctype": DocSalesInvoice,
		"reference_name":    inv,
		"allocated_amount":  amount(ev.Gross),
	}}
	return d, nil
}

// vendorPayment: a Payment Entry (Pay) from the bank to the supplier,
// settling the bill in Refs[0].
func (bw *bookWriter) vendorPayment(ev Event, ref refFunc) (doc, error) {
	if err := positiveGross(ev); err != nil {
		return nil, err
	}
	supplier := bw.rep.Suppliers[ev.Meta.SupplierID]
	if supplier == "" {
		return nil, fmt.Errorf("supplier %q is not in the bootstrap report", ev.Meta.SupplierID)
	}
	if bw.rep.BankAccount == "" {
		return nil, errors.New("no bank account in the bootstrap report")
	}
	creditors, err := bw.account(AccountCreditors)
	if err != nil {
		return nil, err
	}
	inv, err := reference(ev, ref, DocPurchaseInvoice)
	if err != nil {
		return nil, err
	}
	ev.Party = supplier
	d := bw.payment(ev, "Pay", PartySupplier, bw.rep.BankAccount, creditors)
	d["references"] = []doc{{
		"reference_doctype": DocPurchaseInvoice,
		"reference_name":    inv,
		"allocated_amount":  amount(ev.Gross),
	}}
	return d, nil
}

func (bw *bookWriter) payment(ev Event, typ, partyType, paidFrom, paidTo string) doc {
	d := bw.common(ev)
	d["payment_type"] = typ
	d["party_type"] = partyType
	d["party"] = ev.Party
	d["paid_from"] = paidFrom
	d["paid_to"] = paidTo
	d["paid_amount"] = amount(ev.Gross)
	d["received_amount"] = amount(ev.Gross)
	d["source_exchange_rate"] = 1
	d["target_exchange_rate"] = 1
	d["reference_no"] = bankRef(ev)
	d["reference_date"] = ev.Date
	d["custom_remarks"] = 1
	d["remarks"] = ev.Narration
	return d
}

// jeLine is one Journal Entry line.
type jeLine struct {
	account       string
	debit, credit money.Paise
}

// journal: a balanced Journal Entry for payroll, a bank charge, interest or
// an amortisation.
func (bw *bookWriter) journal(ev Event) (doc, error) {
	if err := positiveGross(ev); err != nil {
		return nil, err
	}
	bank := bw.rep.BankAccount
	if bank == "" {
		return nil, errors.New("no bank account in the bootstrap report")
	}
	var lines []jeLine
	switch ev.Kind {
	case EventPayroll:
		sal, err := bw.account(AccountSalaries)
		if err != nil {
			return nil, err
		}
		lines = []jeLine{{account: sal, debit: ev.Gross}, {account: bank, credit: ev.Gross}}
	case EventBankCharge:
		if err := checkGross(ev); err != nil {
			return nil, err
		}
		charges, err := bw.account(AccountBankCharges)
		if err != nil {
			return nil, err
		}
		lines = append(lines, jeLine{account: charges, debit: ev.Taxable})
		for _, t := range []struct {
			label, head string
			amt         money.Paise
		}{
			{"IGST", bw.rep.InputGST.IGST, ev.IGST},
			{"CGST", bw.rep.InputGST.CGST, ev.CGST},
			{"SGST", bw.rep.InputGST.SGST, ev.SGST},
		} {
			if t.amt == 0 {
				continue
			}
			if t.head == "" {
				return nil, fmt.Errorf("no input %s account in the bootstrap report", t.label)
			}
			lines = append(lines, jeLine{account: t.head, debit: t.amt})
		}
		lines = append(lines, jeLine{account: bank, credit: ev.Gross})
	case EventInterest:
		inc, err := bw.account(AccountInterestIncome)
		if err != nil {
			return nil, err
		}
		lines = []jeLine{{account: bank, debit: ev.Gross}, {account: inc, credit: ev.Gross}}
	case EventPrepaidAmortisation:
		accs, err := bw.accounts(ev.Account, AccountPrepaidExpenses)
		if err != nil {
			return nil, err
		}
		lines = []jeLine{{account: accs[0], debit: ev.Gross}, {account: accs[1], credit: ev.Gross}}
	}
	var dr, cr money.Paise
	hitsBank := false
	rows := make([]doc, len(lines))
	for i, l := range lines {
		dr += l.debit
		cr += l.credit
		hitsBank = hitsBank || l.account == bank
		rows[i] = doc{
			"account":                    l.account,
			"debit_in_account_currency":  amount(l.debit),
			"credit_in_account_currency": amount(l.credit),
		}
	}
	if dr != cr {
		return nil, fmt.Errorf("journal does not balance: debit %s, credit %s", dr.Rupees(), cr.Rupees())
	}
	d := bw.common(ev)
	d["voucher_type"] = "Journal Entry"
	d["user_remark"] = ev.Narration
	if hitsBank {
		d["cheque_no"] = bankRef(ev)
		d["cheque_date"] = ev.Date
	}
	d["accounts"] = rows
	return d, nil
}
