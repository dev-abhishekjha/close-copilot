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

func approvedProposal(t *testing.T, st *store.Store) uuid.UUID {
	t.Helper()
	checker := "checker@example.test"
	decided := time.Now().UTC()
	p := store.JournalProposal{
		ID:        uuid.New(),
		CompanyID: "sharma",
		Payload: store.JournalPayload{PostingDate: "2026-09-30", Remark: "lock test", Lines: []store.JournalLine{
			{Account: "A", DebitPaise: 100}, {Account: "B", CreditPaise: 100},
		}},
		Status:    "approved",
		Maker:     "maker@example.test",
		Checker:   &checker,
		DecidedAt: &decided,
	}
	if err := st.CreateJournalProposal(t.Context(), p); err != nil {
		t.Fatalf("CreateJournalProposal: %v", err)
	}
	return p.ID
}

func TestProposalLockIntegration(t *testing.T) {
	st, _ := setupTestStore(t)

	t.Run("serializes and the second sees the first's save", func(t *testing.T) {
		id := approvedProposal(t, st)
		firstLocked := make(chan struct{})
		var (
			mu         sync.Mutex
			order      []string
			secondSaw  *string
			wg         sync.WaitGroup
			errA, errB error
		)
		wg.Go(func() {
			errA = st.WithProposalLock(t.Context(), id, func(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) error {
				close(firstLocked)
				time.Sleep(300 * time.Millisecond) // the second caller must wait this out
				name := "ACC-JV-TEST-00001"
				now := time.Now().UTC()
				p.Status, p.ERPName, p.PostedAt = "posted", &name, &now
				if err := save(ctx, p); err != nil {
					return err
				}
				mu.Lock()
				order = append(order, "first done")
				mu.Unlock()
				return nil
			})
		})
		<-firstLocked
		wg.Go(func() {
			errB = st.WithProposalLock(t.Context(), id, func(_ context.Context, p store.JournalProposal, _ func(context.Context, store.JournalProposal) error) error {
				mu.Lock()
				order = append(order, "second in")
				secondSaw = p.ERPName
				mu.Unlock()
				return nil
			})
		})
		wg.Wait()
		if errA != nil || errB != nil {
			t.Fatalf("errors: first %v, second %v", errA, errB)
		}
		if len(order) != 2 || order[0] != "first done" {
			t.Errorf("order %v, want the first to finish before the second gets the row", order)
		}
		if secondSaw == nil || *secondSaw != "ACC-JV-TEST-00001" {
			t.Errorf("second saw erp_name %v, want the first's", secondSaw)
		}
		got, err := st.GetJournalProposal(t.Context(), id)
		if err != nil || got.Status != "posted" || got.PostedAt == nil {
			t.Errorf("row after: %+v, %v", got, err)
		}
	})

	t.Run("an error from fn rolls back save", func(t *testing.T) {
		id := approvedProposal(t, st)
		boom := errors.New("boom")
		err := st.WithProposalLock(t.Context(), id, func(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) error {
			name := "ACC-JV-ROLLBACK"
			p.Status, p.ERPName = "posted", &name
			if err := save(ctx, p); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err %v, want boom", err)
		}
		got, err := st.GetJournalProposal(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "approved" || got.ERPName != nil {
			t.Errorf("row after rollback: status %q erp_name %v", got.Status, got.ERPName)
		}
	})

	t.Run("a caller cancelled after save still commits", func(t *testing.T) {
		id := approvedProposal(t, st)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		err := st.WithProposalLock(ctx, id, func(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) error {
			name := "ACC-JV-CANCELLED-CALLER"
			p.Status, p.ERPName = "posted", &name
			if err := save(ctx, p); err != nil {
				return err
			}
			cancel()
			return nil
		})
		if err != nil {
			t.Fatalf("WithProposalLock: %v", err)
		}
		got, err := st.GetJournalProposal(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "posted" || got.ERPName == nil || *got.ERPName != "ACC-JV-CANCELLED-CALLER" {
			t.Errorf("row: status %q erp_name %v, want committed", got.Status, got.ERPName)
		}
	})

	t.Run("save after a cancel inside fn still writes", func(t *testing.T) {
		id := approvedProposal(t, st)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		err := st.WithProposalLock(ctx, id, func(ctx context.Context, p store.JournalProposal, save func(context.Context, store.JournalProposal) error) error {
			cancel() // e.g. the client went away right after the ERPNext submit
			name := "ACC-JV-LATE-SAVE"
			p.Status, p.ERPName = "posted", &name
			return save(ctx, p)
		})
		if err != nil {
			t.Fatalf("WithProposalLock: %v", err)
		}
		got, _ := st.GetJournalProposal(t.Context(), id)
		if got.ERPName == nil || *got.ERPName != "ACC-JV-LATE-SAVE" {
			t.Errorf("erp_name %v, want committed", got.ERPName)
		}
	})

	t.Run("missing row is ErrNotFound", func(t *testing.T) {
		called := false
		fn := func(context.Context, store.JournalProposal, func(context.Context, store.JournalProposal) error) error {
			called = true
			return nil
		}
		err := st.WithProposalLock(t.Context(), uuid.New(), fn)
		if !errors.Is(err, store.ErrNotFound) || called {
			t.Errorf("err %v called %v, want ErrNotFound without fn", err, called)
		}
	})
}
