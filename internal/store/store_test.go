package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/abhishekjha/close-copilot/internal/money"
)

func TestOpenInvalidURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := Open(ctx, "invalid-connection-string")
	if err == nil {
		t.Fatal("Open(invalid) want error, got nil")
	}
}

func TestModelsJSONRoundTrip(t *testing.T) {
	// Verify JSON serialization of Finding and Proposal models
	findingID := uuid.New()
	runID := uuid.New()
	amt := money.Paise(118000)

	finding := Finding{
		ID:          findingID,
		RunID:       runID,
		Type:        "unrecorded_bank_charge",
		Severity:    "high",
		Title:       "Bank charge not found in ledger",
		AmountPaise: &amt,
		Keys: map[string]string{
			"bank_txn_id": "BNK-001",
		},
		Evidence: []EvidenceRef{
			{
				Server: "evidence",
				Tool:   "list_bank_lines",
				Args:   json.RawMessage(`{"company":"sharma"}`),
				IDs:    []string{"BNK-001"},
			},
		},
		Citations: []Citation{
			{DocID: "POL-001", Section: "§2.1"},
		},
		Proposal: &JournalProposal{
			ID:        uuid.New(),
			FindingID: &findingID,
			CompanyID: "sharma",
			Payload: JournalPayload{
				PostingDate: "2026-09-30",
				Lines: []JournalLine{
					{Account: "Bank Charges - ST", DebitPaise: 100000},
					{Account: "Input Tax Credit - CGST - ST", DebitPaise: 9000},
					{Account: "Input Tax Credit - SGST - ST", DebitPaise: 9000},
					{Account: "Bank Account - ST", CreditPaise: 118000},
				},
				Remark: "Planted bank charge adjustment",
			},
			Status: "proposed",
			Maker:  "agent",
		},
		Verified: true,
		Status:   "open",
	}

	data, err := json.Marshal(finding)
	if err != nil {
		t.Fatalf("Marshal finding: %v", err)
	}

	var decoded Finding
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal finding: %v", err)
	}

	if decoded.ID != finding.ID {
		t.Errorf("ID: got %v, want %v", decoded.ID, finding.ID)
	}
	if decoded.Type != "unrecorded_bank_charge" {
		t.Errorf("Type: got %q, want unrecorded_bank_charge", decoded.Type)
	}
	if decoded.AmountPaise == nil || *decoded.AmountPaise != amt {
		t.Errorf("AmountPaise: got %v, want %v", decoded.AmountPaise, amt)
	}
	if decoded.Keys["bank_txn_id"] != "BNK-001" {
		t.Errorf("Keys: got %v", decoded.Keys)
	}
	if len(decoded.Evidence) != 1 || decoded.Evidence[0].Tool != "list_bank_lines" {
		t.Errorf("Evidence: got %v", decoded.Evidence)
	}
	if decoded.Proposal == nil || len(decoded.Proposal.Payload.Lines) != 4 {
		t.Errorf("Proposal lines: got %v", decoded.Proposal)
	}
}

func TestDocChunkVector(t *testing.T) {
	vec := make([]float32, 384)
	for i := range vec {
		vec[i] = float32(i) * 0.01
	}
	chunk := DocChunk{
		DocID:       "POL-001",
		DocType:     "policy",
		Content:     "Accounting policy sample",
		ContentHash: "hash123",
		Embedding:   pgvector.NewVector(vec),
	}

	if len(chunk.Embedding.Slice()) != 384 {
		t.Fatalf("Embedding length: got %d, want 384", len(chunk.Embedding.Slice()))
	}
}
