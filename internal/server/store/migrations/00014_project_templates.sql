-- +goose Up
-- A starting point for the next product: how the process runs, what the agents are told, and
-- what the product already believes. Kept as one snapshot rather than five tables, because a
-- template is read whole and written whole, and never joined against anything.
CREATE TABLE project_templates (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    -- {"config": {...}, "packs": ["go-service"], "product": [{"kind","title","content"}],
    --  "repo": {"url","defaultBranch"}}
    payload     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE project_templates;
