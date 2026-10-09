-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE doc_chunks (
    id bigserial PRIMARY KEY,
    doc_id text NOT NULL,
    doc_type text NOT NULL,
    company_id text,
    section text,
    title text,
    content text NOT NULL,
    content_hash text NOT NULL,
    embedding vector(384) NOT NULL,
    tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', coalesce(title, '') || ' ' || content)) STORED
);

CREATE INDEX doc_chunks_embedding ON doc_chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX doc_chunks_tsv ON doc_chunks USING gin (tsv);

-- +goose Down
DROP TABLE IF EXISTS doc_chunks;
