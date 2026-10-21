-- +goose Up
-- A task can now be edited and reopened, and both belong in its history next to the moves
-- between phases: who renamed it, who changed its urgency, who decided closed work was not
-- done after all. The trail stays append-only; only the list of what it may record grows.
ALTER TABLE phase_transitions DROP CONSTRAINT phase_transitions_kind_check;
ALTER TABLE phase_transitions ADD CONSTRAINT phase_transitions_kind_check
    CHECK (kind IN ('create', 'advance', 'rollback', 'handoff', 'auto_block', 'unblock', 'close', 'edit', 'reopen'));

-- +goose Down
ALTER TABLE phase_transitions DROP CONSTRAINT phase_transitions_kind_check;
ALTER TABLE phase_transitions ADD CONSTRAINT phase_transitions_kind_check
    CHECK (kind IN ('create', 'advance', 'rollback', 'handoff', 'auto_block', 'unblock', 'close'));
