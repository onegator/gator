-- +goose Up
-- Where a job runs is the person's choice, not whichever runner asks first. A local job runs
-- on a machine of the person who started it — their Mac, logged in as them — and a cloud job
-- on a runner the server keeps or starts for it. Until now any runner took any job, so a task
-- someone meant to watch on their own Mac could be picked up by a VPS, and the other way round.
ALTER TABLE jobs ADD COLUMN placement text NOT NULL DEFAULT 'local' CHECK (placement IN ('local', 'cloud'));

-- A runner says where it is. Rows written before it said so take it from their location: a
-- VPS is the cloud, a Mac or anything else is somebody's own machine.
ALTER TABLE runners ADD COLUMN placement text NOT NULL DEFAULT 'local' CHECK (placement IN ('local', 'cloud'));
UPDATE runners SET placement = 'cloud' WHERE location = 'vps';

-- Whose machine it is: the person who minted its token. Empty for a token minted on the host,
-- which keeps such a runner shared, as every runner was before.
ALTER TABLE runners ADD COLUMN owner_id uuid REFERENCES users(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE runners DROP COLUMN owner_id;
ALTER TABLE runners DROP COLUMN placement;
ALTER TABLE jobs DROP COLUMN placement;
