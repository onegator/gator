-- +goose Up
-- What is true about a component right now, and what was true before. A scorecard is only
-- worth having if a slip is noticed by somebody who can fix it, so a failing rule remembers
-- since when it has been failing and which task was opened about it.
CREATE TABLE quality_checks (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Null means the rule is about the project as a whole rather than one of its parts.
    component_id  uuid REFERENCES components(id) ON DELETE CASCADE,
    rule          text NOT NULL,
    source        text NOT NULL DEFAULT 'builtin',
    status        text NOT NULL CHECK (status IN ('pass', 'fail', 'unknown')),
    detail        text NOT NULL DEFAULT '',
    -- How much this rule counts toward the score. A rule everybody ignores is worth nothing.
    weight        integer NOT NULL DEFAULT 1 CHECK (weight >= 0),
    failing_since timestamptz,
    task_id       uuid REFERENCES tasks(id) ON DELETE SET NULL,
    checked_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (project_id, component_id, rule)
);

-- The trend. One row per component per sweep: the point of a scorecard is the direction,
-- not today's number.
CREATE TABLE quality_scores (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    component_id uuid REFERENCES components(id) ON DELETE CASCADE,
    earned       integer NOT NULL,
    possible     integer NOT NULL,
    checked_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX quality_scores_trend_idx ON quality_scores (project_id, component_id, checked_at DESC);
CREATE INDEX quality_checks_failing_idx ON quality_checks (project_id) WHERE status = 'fail';

-- +goose Down
DROP INDEX quality_checks_failing_idx;
DROP INDEX quality_scores_trend_idx;
DROP TABLE quality_scores;
DROP TABLE quality_checks;
