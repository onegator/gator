-- +goose Up
-- How many times this job's turn was sent back to work before it was allowed to end. A job
-- ends because the agent says so; something watching the work — a plugin that knows the tests
-- never ran, the core that knows a plan was left untouched — may say "not yet". The count is
-- kept here rather than derived, because the limit that stops an objection loop has to be
-- decided by the server, not by the runner asking the question.
ALTER TABLE jobs ADD COLUMN objections integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE jobs DROP COLUMN objections;
