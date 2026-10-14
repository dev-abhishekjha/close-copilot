package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The inserts and lookups need Postgres (proposals_integration_test.go);
// these are the checks made before any database call.

func TestCheckNewProposal(t *testing.T) {
	fid := uuid.New()
	nilFID := uuid.Nil
	s := "x"
	now := time.Now()
	valid := func() JournalProposal {
		return JournalProposal{ID: uuid.New(), FindingID: &fid, CompanyID: "sharma", Status: "proposed", Maker: "asha"}
	}
	cases := []struct {
		name   string
		mutate func(*JournalProposal)
		ok     bool
	}{
		{"valid", func(*JournalProposal) {}, true},
		{"nil id", func(p *JournalProposal) { p.ID = uuid.Nil }, false},
		{"no finding", func(p *JournalProposal) { p.FindingID = nil }, false},
		{"nil finding", func(p *JournalProposal) { p.FindingID = &nilFID }, false},
		{"no company", func(p *JournalProposal) { p.CompanyID = "" }, false},
		{"no maker", func(p *JournalProposal) { p.Maker = "" }, false},
		{"approved", func(p *JournalProposal) { p.Status = "approved" }, false},
		{"posted", func(p *JournalProposal) { p.Status = "posted" }, false},
		{"checker set", func(p *JournalProposal) { p.Checker = &s }, false},
		{"reason set", func(p *JournalProposal) { p.Reason = &s }, false},
		{"erp name set", func(p *JournalProposal) { p.ERPName = &s }, false},
		{"decided set", func(p *JournalProposal) { p.DecidedAt = &now }, false},
		{"posted at set", func(p *JournalProposal) { p.PostedAt = &now }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid()
			tc.mutate(&p)
			err := checkNewProposal(p)
			if (err == nil) != tc.ok {
				t.Errorf("checkNewProposal: %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestCreateProposalFromFindingRefusesBeforeDB(t *testing.T) {
	var s Store // not open: any database call would panic on the nil pool
	if _, err := s.CreateProposalFromFinding(t.Context(), JournalProposal{ID: uuid.New(), Status: "proposed"}); err == nil {
		t.Error("want an error for a proposal without a finding")
	}
}

func TestProposalListLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{-1, MaxProposalList}, {0, MaxProposalList}, {1, 1}, {50, 50},
		{MaxProposalList, MaxProposalList}, {MaxProposalList + 1, MaxProposalList}, {10000, MaxProposalList},
	}
	for _, tc := range cases {
		if got := proposalListLimit(tc.in); got != tc.want {
			t.Errorf("proposalListLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestWithProposalFindingLockRefusesBadArgs(t *testing.T) {
	type fnT = func(context.Context, JournalProposal, FindingStatusFunc, func(context.Context, JournalProposal) error) error
	noop := func(context.Context, JournalProposal, FindingStatusFunc, func(context.Context, JournalProposal) error) error {
		return nil
	}
	var s Store // not open: any database call would panic on the nil pool
	cases := []struct {
		name string
		id   uuid.UUID
		fn   fnT
	}{
		{"nil fn", uuid.New(), nil},
		{"nil id", uuid.Nil, noop},
		{"store not open", uuid.New(), noop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.WithProposalFindingLock(t.Context(), tc.id, tc.fn); err == nil {
				t.Error("want an error")
			}
		})
	}
}
