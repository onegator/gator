-- +goose Up
-- One row: what this workspace has decided for every project in it. Until now there were
-- defaults in the binary and overrides per project, and nothing in between — so a team with its
-- own way of working had to paste it into each project and remember every change.
CREATE TABLE workspace_settings (
    id             boolean PRIMARY KEY DEFAULT true CHECK (id),
    process_config jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

INSERT INTO workspace_settings (id) VALUES (true);

-- +goose Down
DROP TABLE workspace_settings;
