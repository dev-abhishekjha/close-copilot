//go:build integration

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// liveFinding stores a finding of a new run and returns its ID.
func liveFinding(t *testing.T, st *store.Store) uuid.UUID {
	t.Helper()
	run := newRun(t, st)
	f := testFinding(t, st, run, "TXN-PROP-"+uuid.NewString()[:8])
	if err := st.CreateFinding(context.Background(), f); err != nil {
		t.Fatalf("CreateFinding: %v", err)
	}
	return f.ID
}

func newProposal(findingID uuid.UUID, created time.Time) store.JournalProposal {
	return store.JournalProposal{
		ID:        uuid.New(),
		FindingID: &findingID,
		CompanyID: "testcorp",
		Payload: store.JournalPayload{PostingDate: "2026-08-31", Remark: "bank charge", Lines: []store.JournalLine{
			{Account: "Bank Charges - TC", DebitPaise: 29500}, {Account: "Bank - TC", CreditPaise: 29500},
		}},
		Status:    "proposed",
		Maker:     "asha",
		CreatedAt: created,
	}
}

// reject marks a proposal rejected through the row lock, as the approvals
// service does.
func reject(t *testing.T, st *store.Store, id uuid.UUID) {
	t.Helper()
	err := st.WithProposalLock(context.Background(), id, func(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) error {
		checker, reason, now := "ravi", "wrong account", time.Now().UTC()
		p.Status, p.Checker, p.Reason, p.DecidedAt = "rejected", &checker, &reason, &now
		return save(ctx, p)
	})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
}

func TestProposalsIntegration(t *testing.T) {
	ctx := context.Background()
	st, _ := setupTestStore(t)

	t.Run("create and read back", func(t *testing.T) {
		fid := liveFinding(t, st)
		p := newProposal(fid, time.Now().UTC().Truncate(time.Microsecond))
		got, err := st.CreateProposalFromFinding(ctx, p)
		if err != nil {
			t.Fatalf("CreateProposalFromFinding: %v", err)
		}
		if got.ID != p.ID || got.FindingID == nil || *got.FindingID != fid || got.Status != "proposed" || got.Maker != "asha" ||
			len(got.Payload.Lines) != 2 || got.Payload.Lines[0].DebitPaise != 29500 || got.ERPName != nil || got.Checker != nil {
			t.Errorf("created %+v", got)
		}
		live, ok, err := st.GetProposalByFinding(ctx, fid)
		if err != nil || !ok || live.ID != p.ID {
			t.Errorf("GetProposalByFinding: %+v ok=%v err=%v", live, ok, err)
		}
		if _, ok, err := st.GetProposalByFinding(ctx, uuid.New()); err != nil || ok {
			t.Errorf("unknown finding: ok=%v err=%v, want none", ok, err)
		}
	})

	t.Run("list by status oldest first", func(t *testing.T) {
		base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		var ids []uuid.UUID
		for i := range 3 {
			p := newProposal(liveFinding(t, st), base.Add(time.Duration(i)*time.Second))
			if _, err := st.CreateProposalFromFinding(ctx, p); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, p.ID)
		}
		reject(t, st, ids[1])
		proposed, err := st.ListProposalsByStatus(ctx, "proposed", 0)
		if err != nil {
			t.Fatalf("ListProposalsByStatus: %v", err)
		}
		pos := map[uuid.UUID]int{}
		for i, p := range proposed {
			if p.Status != "proposed" {
				t.Errorf("listed status %q", p.Status)
			}
			if len(p.Payload.Lines) == 0 {
				t.Errorf("listed %s without payload", p.ID)
			}
			pos[p.ID] = i
		}
		i0, ok0 := pos[ids[0]]
		i2, ok2 := pos[ids[2]]
		if !ok0 || !ok2 || i0 >= i2 {
			t.Errorf("positions %v of %v, want ids[0] before ids[2]", pos, ids)
		}
		if _, ok := pos[ids[1]]; ok {
			t.Error("rejected proposal listed as proposed")
		}
		rejected, err := st.ListProposalsByStatus(ctx, "rejected", 200)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range rejected {
			found = found || p.ID == ids[1]
		}
		if !found {
			t.Error("rejected proposal not listed")
		}
		one, err := st.ListProposalsByStatus(ctx, "proposed", 1)
		if err != nil || len(one) != 1 {
			t.Errorf("limit 1: %d rows, %v", len(one), err)
		}
	})

	t.Run("one live per finding", func(t *testing.T) {
		fid := liveFinding(t, st)
		first := newProposal(fid, time.Now().UTC())
		if _, err := st.CreateProposalFromFinding(ctx, first); err != nil {
			t.Fatal(err)
		}
		existing, err := st.CreateProposalFromFinding(ctx, newProposal(fid, time.Now().UTC()))
		if !errors.Is(err, store.ErrProposalExists) {
			t.Fatalf("second create: %v, want ErrProposalExists", err)
		}
		if existing.ID != first.ID {
			t.Errorf("existing %s, want %s", existing.ID, first.ID)
		}
	})

	t.Run("posted keeps the finding taken", func(t *testing.T) {
		fid := liveFinding(t, st)
		first := newProposal(fid, time.Now().UTC())
		if _, err := st.CreateProposalFromFinding(ctx, first); err != nil {
			t.Fatal(err)
		}
		err := st.WithProposalLock(ctx, first.ID, func(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) error {
			name, now := "ACC-JV-PROP-1", time.Now().UTC()
			p.Status, p.ERPName, p.PostedAt = "posted", &name, &now
			return save(ctx, p)
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateProposalFromFinding(ctx, newProposal(fid, time.Now().UTC())); !errors.Is(err, store.ErrProposalExists) {
			t.Errorf("create after posted: %v, want ErrProposalExists", err)
		}
	})

	t.Run("rejected frees the finding", func(t *testing.T) {
		fid := liveFinding(t, st)
		first := newProposal(fid, time.Now().UTC())
		if _, err := st.CreateProposalFromFinding(ctx, first); err != nil {
			t.Fatal(err)
		}
		reject(t, st, first.ID)
		if _, ok, err := st.GetProposalByFinding(ctx, fid); err != nil || ok {
			t.Errorf("after reject: ok=%v err=%v, want none live", ok, err)
		}
		second := newProposal(fid, time.Now().UTC())
		if _, err := st.CreateProposalFromFinding(ctx, second); err != nil {
			t.Fatalf("create after reject: %v", err)
		}
		live, ok, err := st.GetProposalByFinding(ctx, fid)
		if err != nil || !ok || live.ID != second.ID {
			t.Errorf("live after reject: %+v ok=%v err=%v", live.ID, ok, err)
		}
	})

	t.Run("two concurrent creates yield one row", func(t *testing.T) {
		fid := liveFinding(t, st)
		const n = 4
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			oks  int
			dups int
			errs []error
		)
		for range n {
			wg.Go(func() {
				_, err := st.CreateProposalFromFinding(ctx, newProposal(fid, time.Now().UTC()))
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					oks++
				case errors.Is(err, store.ErrProposalExists):
					dups++
				default:
					errs = append(errs, err)
				}
			})
		}
		wg.Wait()
		if oks != 1 || dups != n-1 || len(errs) != 0 {
			t.Errorf("oks %d dups %d errs %v, want 1 and %d", oks, dups, errs, n-1)
		}
		var count int
		if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM journal_proposals WHERE finding_id = $1`, fid).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("%d rows for the finding, want 1", count)
		}
	})

	// The first approval reads the finding's status under the proposal
	// lock with FOR SHARE: a dismissal that starts after the read waits
	// until the approval commits, and one committed before it is seen.
	t.Run("finding status read blocks a dismissal until commit", func(t *testing.T) {
		fid := liveFinding(t, st)
		p := newProposal(fid, time.Now().UTC())
		if _, err := st.CreateProposalFromFinding(ctx, p); err != nil {
			t.Fatal(err)
		}
		read, release := make(chan struct{}), make(chan struct{})
		var (
			seen  string
			found bool
			errA  error
		)
		aDone := make(chan struct{})
		go func() {
			defer close(aDone)
			errA = st.WithProposalFindingLock(ctx, p.ID, func(ctx context.Context, p store.JournalProposal, findingStatus store.FindingStatusFunc, save func(context.Context, store.JournalProposal) error) error {
				var err error
				seen, found, err = findingStatus(ctx)
				close(read)
				if err != nil {
					return err
				}
				<-release
				checker, now := "ravi", time.Now().UTC()
				p.Status, p.Checker, p.DecidedAt = "approved", &checker, &now
				return save(ctx, p)
			})
		}()
		<-read
		bDone := make(chan error, 1)
		go func() { bDone <- st.SetFindingStatus(ctx, fid, "dismissed") }()
		select {
		case err := <-bDone:
			close(release)
			<-aDone
			t.Fatalf("dismissal finished (%v) while the approval held the finding's share lock", err)
		case <-time.After(500 * time.Millisecond):
		}
		close(release)
		<-aDone
		if errA != nil || seen != "open" || !found {
			t.Fatalf("approval: saw %q found=%v err=%v, want open", seen, found, errA)
		}
		select {
		case err := <-bDone:
			if err != nil {
				t.Fatalf("dismissal after commit: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("dismissal still blocked after the approval committed")
		}
		got, err := st.GetJournalProposal(ctx, p.ID)
		if err != nil || got.Status != "approved" {
			t.Errorf("proposal %+v %v, want approved", got.Status, err)
		}
		f, err := st.GetFinding(ctx, fid)
		if err != nil || f.Status != "dismissed" {
			t.Errorf("finding %q %v, want dismissed", f.Status, err)
		}
	})

	t.Run("a committed dismissal is seen under the lock", func(t *testing.T) {
		fid := liveFinding(t, st)
		p := newProposal(fid, time.Now().UTC())
		if _, err := st.CreateProposalFromFinding(ctx, p); err != nil {
			t.Fatal(err)
		}
		if err := st.SetFindingStatus(ctx, fid, "dismissed"); err != nil {
			t.Fatal(err)
		}
		saved := false
		err := st.WithProposalFindingLock(ctx, p.ID, func(ctx context.Context, p store.JournalProposal, findingStatus store.FindingStatusFunc, save func(context.Context, store.JournalProposal) error) error {
			status, found, err := findingStatus(ctx)
			if err != nil {
				return err
			}
			if !found || status != "dismissed" {
				t.Errorf("finding status %q found=%v, want dismissed", status, found)
			}
			if status != "open" {
				return errors.New("finding not open")
			}
			saved = true
			return save(ctx, p)
		})
		if err == nil || saved {
			t.Errorf("approval of a dismissed finding: err=%v saved=%v", err, saved)
		}
		if got, _ := st.GetJournalProposal(ctx, p.ID); got.Status != "proposed" {
			t.Errorf("proposal %s, want proposed", got.Status)
		}
	})

	// A retry that doesn't read the finding holds no lock on it: a
	// dismissal goes through while the proposal row is locked.
	t.Run("no read, no finding lock", func(t *testing.T) {
		fid := liveFinding(t, st)
		p := newProposal(fid, time.Now().UTC())
		if _, err := st.CreateProposalFromFinding(ctx, p); err != nil {
			t.Fatal(err)
		}
		err := st.WithProposalFindingLock(ctx, p.ID, func(ctx context.Context, _ store.JournalProposal, _ store.FindingStatusFunc, _ func(context.Context, store.JournalProposal) error) error {
			dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return st.SetFindingStatus(dctx, fid, "dismissed")
		})
		if err != nil {
			t.Fatalf("dismissal under the proposal lock: %v", err)
		}
	})

	t.Run("finding lock of a missing proposal", func(t *testing.T) {
		err := st.WithProposalFindingLock(ctx, uuid.New(), func(context.Context, store.JournalProposal, store.FindingStatusFunc, func(context.Context, store.JournalProposal) error) error {
			t.Error("fn called for a missing proposal")
			return nil
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err %v, want ErrNotFound", err)
		}
	})
}
