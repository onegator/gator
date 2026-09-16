-- +goose Up
-- Devices a person signed in on, for push notifications.
CREATE TABLE devices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token        text NOT NULL UNIQUE,
    platform     text NOT NULL CHECK (platform IN ('ios', 'macos')),
    app_version  text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    -- set when Apple says the token is gone; the device stops receiving until it registers again
    failed_at    timestamptz
);
CREATE INDEX devices_user_idx ON devices (user_id) WHERE failed_at IS NULL;

-- One durable read cursor over the outbox per consumer, so no consumer loses an event to a
-- restart. Replaces the plugin host's own table.
CREATE TABLE event_cursors (
    name          text PRIMARY KEY,
    last_event_id bigint NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now()
);
INSERT INTO event_cursors (name, last_event_id)
SELECT 'plugins', last_event_id FROM plugin_event_cursor WHERE id = 1;
DROP TABLE plugin_event_cursor;

-- +goose Down
CREATE TABLE plugin_event_cursor (
    id            integer PRIMARY KEY CHECK (id = 1),
    last_event_id bigint NOT NULL
);
INSERT INTO plugin_event_cursor (id, last_event_id)
SELECT 1, last_event_id FROM event_cursors WHERE name = 'plugins';
DROP TABLE event_cursors;
DROP TABLE devices;
