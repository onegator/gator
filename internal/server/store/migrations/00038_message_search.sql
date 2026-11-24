-- +goose Up
-- Full-text search over what people wrote. The 'simple' configuration does no stemming, which is
-- right for a team writing in Polish and English at once. An expression index rather than a
-- stored tsvector column: the same search, without carrying the vector in every message read.
CREATE INDEX messages_search_idx ON messages USING GIN (to_tsvector('simple', body)) WHERE deleted_at IS NULL;

-- Finding a channel by part of its name.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX conversations_name_trgm_idx ON conversations USING GIN (name gin_trgm_ops) WHERE kind = 'channel';

-- +goose Down
DROP INDEX conversations_name_trgm_idx;
DROP EXTENSION IF EXISTS pg_trgm;
DROP INDEX messages_search_idx;
