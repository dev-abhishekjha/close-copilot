package checks

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
)

type fakeCheck struct {
	name     string
	delay    time.Duration
	findings []Finding
	err      error
	onRun    func()
}

func (f *fakeCheck) Name() string {
	return f.name
}

func (f *fakeCheck) Run(_ context.Context, _ Inputs) ([]Finding, error) {
	if f.onRun != nil {
		f.onRun()
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.findings, nil
}

type fakeStore struct {
	mu      sync.Mutex
	saved   []Finding
	saveErr error
	txCount int
}

func (s *fakeStore) CreateFindings(_ context.Context, findings []Finding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txCount++
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved = append(s.saved, findings...)
	return nil
}

func TestParallelFakeChecks(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()

	var active int32
	var maxActive int32

	numChecks := 8
	checks := make([]Check, numChecks)
	for i := 0; i < numChecks; i++ {
		checks[i] = &fakeCheck{
			name:  uuid.New().String()[:8],
			delay: 20 * time.Millisecond,
			onRun: func() {
				cur := atomic.AddInt32(&active, 1)
				for {
					old := atomic.LoadInt32(&maxActive)
					if cur <= old || atomic.CompareAndSwapInt32(&maxActive, old, cur) {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
				atomic.AddInt32(&active, -1)
			},
		}
	}

	st := &fakeStore{}
	runner := NewRunner(st)
	runner.SetLimit(4)

	findings, err := runner.Run(ctx, runID, Inputs{}, checks...)
	if err != nil {
		t.Fatalf("Runner.Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}

	maxConcurrent := atomic.LoadInt32(&maxActive)
	if maxConcurrent > 4 {
		t.Errorf("max active concurrency was %d, expected <= 4", maxConcurrent)
	}
	if maxConcurrent < 2 {
		t.Errorf("expected parallel execution (max active >= 2), got %d", maxConcurrent)
	}
}

func TestDuplicateFindingsCollapse(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()

	amt := money.Paise(50000)
	// Check 1 produces unrecorded bank charge with evidence 1
	c1 := &fakeCheck{
		name: "check1",
		findings: []Finding{
			{
				Type:        TypeUnrecordedBankCharge,
				Severity:    SeverityMedium,
				Title:       "Bank Charge 1",
				AmountPaise: &amt,
				Keys:        map[string]string{"bank_txn_id": "TXN-999"},
				Evidence:    []EvidenceRef{EvidenceEvidence("list_bank_lines", nil, "TXN-999")},
			},
		},
	}

	// Check 2 produces the identical unrecorded bank charge with additional evidence
	c2 := &fakeCheck{
		name: "check2",
		findings: []Finding{
			{
				Type:        TypeUnrecordedBankCharge,
				Severity:    SeverityMedium,
				Title:       "Bank Charge 2",
				AmountPaise: &amt,
				Keys:        map[string]string{"bank_txn_id": "TXN-999"},
				Evidence:    []EvidenceRef{BooksEvidence("list_gl_entries", nil, "GLE-123")},
			},
		},
	}

	// Check 3 produces a different finding
	c3 := &fakeCheck{
		name: "check3",
		findings: []Finding{
			{
				Type:     TypeMissingAccrual,
				Severity: SeverityHigh,
				Title:    "Missing Rent Accrual",
				Keys:     map[string]string{"supplier": "Landlord", "month": "2026-09"},
				Evidence: []EvidenceRef{BooksEvidence("list_recurring_suppliers", nil, "Landlord")},
			},
		},
	}

	st := &fakeStore{}
	runner := NewRunner(st)

	results, err := runner.Run(ctx, runID, Inputs{}, c1, c2, c3)
	if err != nil {
		t.Fatalf("Runner.Run: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("got %d findings after deduplication, want 2", len(results))
	}

	// Verify the bank charge collapsed into one finding with both evidence refs
	var bankCharge *Finding
	for i := range results {
		if results[i].Type == TypeUnrecordedBankCharge {
			bankCharge = &results[i]
		}
	}

	if bankCharge == nil {
		t.Fatal("expected unrecorded_bank_charge in results")
	}
	if len(bankCharge.Evidence) != 2 {
		t.Errorf("bank charge evidence len = %d, want 2 merged evidence refs", len(bankCharge.Evidence))
	}

	// Verify persistence in one transaction
	if st.txCount != 1 {
		t.Errorf("store txCount = %d, want 1 transaction", st.txCount)
	}
	if len(st.saved) != 2 {
		t.Errorf("store saved len = %d, want 2", len(st.saved))
	}
	for _, f := range st.saved {
		if f.RunID != runID {
			t.Errorf("saved finding RunID = %s, want %s", f.RunID, runID)
		}
		if f.ID == uuid.Nil {
			t.Error("saved finding has nil UUID")
		}
	}
}

func TestCheckErrorPropagation(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()

	failErr := errors.New("books server unreachable")
	c1 := &fakeCheck{name: "goodCheck"}
	c2 := &fakeCheck{name: "failingCheck", err: failErr}

	st := &fakeStore{}
	runner := NewRunner(st)

	_, err := runner.Run(ctx, runID, Inputs{}, c1, c2)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, failErr) {
		t.Errorf("expected wrapped failErr, got %v", err)
	}
}

func TestEmptyChecks(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()
	runner := NewRunner(nil)

	results, err := runner.Run(ctx, runID, Inputs{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results, got %v", results)
	}
}
