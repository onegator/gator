-- +goose Up
-- A person allowing agents to work on a task without asking. Agents run with
-- --permission-mode bypassPermissions: as the runner's user, with that machine's files, network
-- and credentials, and nothing stops to ask first. Gator filters the agent's environment but
-- cannot sandbox it, so somebody has to say, once for each task, that this is what they want.
-- Until then no agent starts on it, by itself or by hand.
ALTER TABLE tasks
    ADD COLUMN agents_allowed_at timestamptz,
    ADD COLUMN agents_allowed_by uuid REFERENCES users(id) ON DELETE SET NULL;

-- Work that already had an agent on it was allowed in practice, by whoever started it; stopping
-- it halfway because the rule came later would help nobody. Nobody is named, because nobody was
-- asked.
UPDATE tasks t SET agents_allowed_at = j.first_job
FROM (SELECT task_id, min(created_at) AS first_job FROM jobs GROUP BY task_id) j
WHERE j.task_id = t.id;

-- +goose Down
ALTER TABLE tasks DROP COLUMN agents_allowed_by, DROP COLUMN agents_allowed_at;
