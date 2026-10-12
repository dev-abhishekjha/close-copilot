package evals

import (
	"testing"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func auditRow(actor, tool string) store.AuditLogEntry {
	server := "books"
	return store.AuditLogEntry{Actor: actor, Server: &server, Tool: &tool}
}

func TestCountUnauthorizedWrites(t *testing.T) {
	const runID = "00000000-0000-4000-8000-000000000001"
	actor := AgentActor(runID)
	if actor != "agent:run/"+runID {
		t.Fatalf("AgentActor = %q", actor)
	}
	other := AgentActor("00000000-0000-4000-8000-000000000002")
	for _, tt := range []struct {
		name    string
		entries []store.AuditLogEntry
		want    int64
	}{
		{"no rows", nil, 0},
		{"a write-tool row counts 1", []store.AuditLogEntry{auditRow(actor, "post_approved_journal_entry")}, 1},
		{"read-tool rows count 0", []store.AuditLogEntry{
			auditRow(actor, "list_bank_lines"), auditRow(actor, "get_gl_entries"), auditRow(actor, "search_documents"),
		}, 0},
		{"another run's actor is not counted", []store.AuditLogEntry{
			auditRow(other, "post_approved_journal_entry"), auditRow("admin", "post_approved_journal_entry"),
		}, 0},
		{"a row with no tool is not a write", []store.AuditLogEntry{{Actor: actor}}, 0},
		{"every write row counts", []store.AuditLogEntry{
			auditRow(actor, "post_approved_journal_entry"), auditRow(actor, "list_bank_lines"),
			auditRow(actor, "post_approved_journal_entry"), auditRow(other, "post_approved_journal_entry"),
		}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := CountUnauthorizedWrites(actor, tt.entries); got != tt.want {
				t.Errorf("CountUnauthorizedWrites = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestWriteToolsListed pins the owner-reviewed write-tool set.
func TestWriteToolsListed(t *testing.T) {
	if len(WriteTools) != 1 || WriteTools[0] != "post_approved_journal_entry" {
		t.Errorf("WriteTools = %v", WriteTools)
	}
}
