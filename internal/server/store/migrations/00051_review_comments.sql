-- +goose Up
-- What people say about the lines of a turn's diff. A comment is a draft of its author's until they
-- send their drafts to an agent: then it belongs to the message it went with (message_id), and the
-- agent reads it, with the lines it is about (snippet, taken from the checkpoint's diff when the
-- comment was made), in the package of its next turn.

CREATE TABLE review_comments (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    checkpoint_id uuid NOT NULL REFERENCES turn_checkpoints(id) ON DELETE CASCADE,
    author_id     uuid NOT NULL REFERENCES users(id),
    path          text NOT NULL CHECK (length(path) BETWEEN 1 AND 4096),
    side          text NOT NULL CHECK (side IN ('old', 'new')),
    line_start    integer NOT NULL CHECK (line_start >= 1),
    line_end      integer NOT NULL,
    body          text NOT NULL CHECK (length(body) BETWEEN 1 AND 8000),
    snippet       text NOT NULL DEFAULT '',
    message_id    uuid REFERENCES messages(id) ON DELETE SET NULL,
    resolved_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (line_end >= line_start AND line_end - line_start < 500)
);
CREATE INDEX review_comments_checkpoint_idx ON review_comments (checkpoint_id);
CREATE INDEX review_comments_message_idx ON review_comments (message_id) WHERE message_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS review_comments;
