# ADR 0003: Postgres and pgvector instead of a dedicated vector database

- Status: accepted
- Date: 2026-10-14
- Decides: where document chunks, their embeddings and the search index live (CC-802 to CC-805)

## Context

The explainer cites the company's policy manual, contracts and close notes, so the system needs document search. Postgres was already required for the evidence (`bank_lines`, `gstr2b_entries`), the runs (`close_runs`, `findings`, `journal_proposals`, `audit_log`) and the data plane (`run_steps`, `artifacts`, `llm_calls`).

The corpus is small. The ingested set behind the benchmarks is 378 chunks: 1 policy, 34 contracts and 6 notes ([docs/benchmarks.md](../docs/benchmarks.md)). The hard requirement is tenancy: a company's search must never rank another company's chunk. CC-804 makes this an invariant, with the company filter inside the query, before ranking.

## Options

1. **A dedicated vector database beside Postgres.** It would have purpose-built indexes and filtering. It lost because it means a second datastore to run, back up and keep in step with Postgres. The tenant filter, full-text search and the rest of the data would sit in two systems, and a hosted service would send document text to another third party. It would also add a dependency the G5 licence check has to clear. At a few hundred chunks it brings no measurable gain.
2. **An in-process index in the Go service.** There is nothing to deploy. It lost because the index would have to be rebuilt on every start and would have no transactions with the rest of the data. There would be no full-text half for hybrid search, and the evidence server, which serves `search_documents`, would hold the corpus in memory.
3. **Postgres 17 with pgvector, in the same database (chosen).**

## Decision

- `doc_chunks` holds each chunk's text, a 384-dimension embedding (TEI `bge-small-en-v1.5`, run locally) with an HNSW cosine index, and a generated `tsvector` column for full-text search (`migrations/0003_docs.sql`).
- `internal/retrieval` runs hybrid search. It merges vector and full-text candidates with reciprocal rank fusion, then reranks them with TEI `bge-reranker-base`. Neither model is generative, and neither leaves the deployment.
- The company filter (the asking company, plus the shared policy with no company) and the document-type filter are applied inside the SQL, before any ranking. An empty company is an error, never "all companies".

## Consequences

- **One database** to run, migrate (goose) and back up, with document chunks under the same transactions and roles as everything else.
- **Measured retrieval** (benchmark 3, 54 golden queries with expectations): recall@5 was 0.926 for vector only, 0.926 for hybrid and 0.944 for hybrid plus rerank. The 6 tenant-boundary queries returned zero cross-company hits in every mode. The differences between modes are one query or less, so the modes aren't really separable on this set.
- **Citation quality is the weak point, not the index.** With top-3 retrieval on the current query, the explainer cited an expected section in 2 of 6 golden cases (benchmark 2). That is a query-building problem that no change of store would fix.
- **Unknown at scale.** Nothing here has been tested beyond one small corpus and two companies. With many tenants and a large corpus, filtering inside an approximate index scan can return fewer candidates than asked for, and the HNSW parameters would need tuning.
- **What would change this decision:** a corpus or tenant count large enough that filtered recall or latency in Postgres falls short in a measured benchmark, or a need for vector features pgvector lacks.
