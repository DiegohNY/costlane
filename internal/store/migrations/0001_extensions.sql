-- +goose Up
-- btree_gist backs the exclusion constraints that keep price and alias
-- validity ranges from overlapping. It is a trusted extension from
-- PostgreSQL 13 onward, so the database owner can install it without
-- superuser rights.
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- +goose Down
DROP EXTENSION IF EXISTS btree_gist;
