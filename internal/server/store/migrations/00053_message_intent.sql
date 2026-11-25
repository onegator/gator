-- +goose Up
-- What a person meant a message for while an agent was answering: a correction of the turn that
-- runs (steer), or the next thing to do once it ends (queued). A queued message asks nobody until
-- the turn ends; then it starts the next turn (turn_start), or a person sends it at once as a steer
-- (promoted_to_steer). NULL is a message written with no turn running.

ALTER TABLE messages ADD COLUMN input_intent text
    CHECK (input_intent IN ('turn_start', 'steer', 'queued', 'promoted_to_steer'));
CREATE INDEX messages_queued_idx ON messages (conversation_id, seq) WHERE input_intent = 'queued' AND deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS messages_queued_idx;
ALTER TABLE messages DROP COLUMN IF EXISTS input_intent;
