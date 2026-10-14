-- +goose Up
-- Maker-checker approvals (CC-1004): a finding has at most one live journal
-- proposal. A rejected proposal frees the finding for a new one; a posted
-- one does not.
CREATE UNIQUE INDEX journal_proposals_one_live_per_finding
    ON journal_proposals (finding_id)
    WHERE finding_id IS NOT NULL AND status <> 'rejected';

-- +goose Down
DROP INDEX IF EXISTS journal_proposals_one_live_per_finding;
