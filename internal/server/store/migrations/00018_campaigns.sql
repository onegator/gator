-- +goose Up
-- A campaign is one decision carried out in many places: bump this dependency everywhere,
-- move every service off that endpoint. Each target gets its own task with its own gate, so
-- one repository being awkward does not hold up the other nine.
CREATE TABLE campaign_targets (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    -- Where the work happens. A target is a component of this project or another project
    -- entirely; target_key is how a person named it.
    target_key  text NOT NULL,
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id     uuid REFERENCES tasks(id) ON DELETE SET NULL,
    -- Skipping is a decision, so it carries a reason. Without one, a campaign could be
    -- declared finished by quietly dropping the hard half.
    skipped_at  timestamptz,
    skip_reason text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (campaign_id, target_key)
);

CREATE INDEX campaign_targets_task_idx ON campaign_targets (task_id);

-- +goose Down
DROP INDEX campaign_targets_task_idx;
DROP TABLE campaign_targets;
