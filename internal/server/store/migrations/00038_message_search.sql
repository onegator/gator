-- +goose Up
-- Full-text search over what people wrote. The 'simple' configuration does no stemming, which is
-- right for a team writing in Polish and English at once. An expression index rather than a
-- stored tsvector column: the same search, without carrying the vector in every message read.
CREATE INDEX messages_search_idx ON messages USING GIN (to_tsvector('simple', body)) WHERE deleted_at IS NULL;

-- Finding a channel by part of its name. Not a partial index on kind = 'channel': the list asks
-- for its kind as a parameter, which a partial index cannot serve; other kinds have no name.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX conversations_name_trgm_idx ON conversations USING GIN (name gin_trgm_ops);

-- +goose Down
-- pg_trgm stays: it may have been there before, and something else may use it by now.
DROP INDEX conversations_name_trgm_idx;
DROP INDEX messages_search_idx;
