package store

import (
	"context"
	"testing"

	"time"

	"github.com/google/uuid"
)

// The locking itself needs Postgres (proposal_lock_integration_test.go);
// these are the checks made before any database call.

func TestWithProposalLockRefusesBadArgs(t *testing.T) {
	noop := func(context.Context, JournalProposal, func(context.Context, JournalProposal) error) error { return nil }
	var s Store // not open: any database call would panic on the nil pool
	cases := []struct {
		name string
		id   uuid.UUID
		fn   func(context.Context, JournalProposal, func(context.Context, JournalProposal) error) error
	}{
		{"nil fn", uuid.New(), nil},
		{"nil id", uuid.Nil, noop},
		{"store not open", uuid.New(), noop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.WithProposalLock(t.Context(), tc.id, tc.fn); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestDetachedOutlivesCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d, stop := detached(ctx)
	defer stop()
	if err := d.Err(); err != nil {
		t.Fatalf("detached ctx: %v, want live", err)
	}
	dl, ok := d.Deadline()
	if !ok {
		t.Fatal("detached ctx has no deadline")
	}
	if left := time.Until(dl); left <= 0 || left > ProposalSaveTimeout {
		t.Errorf("deadline in %v, want within %v", left, ProposalSaveTimeout)
	}
}
