# Benchmarks: the trade-off write-up

Method note. All data is synthetic. Every number below was measured on one laptop on 2026-10-14 at commit `52cca50` (branch `cc-906-benchmarks`, base of CC-906), with `LLM_PROVIDER=claude-cli`, `LLM_MODEL_FAST=haiku`, `LLM_MODEL_STRONG=sonnet`. Samples are small: benchmarks 2 and 4 use one close, `suite-v1` `sharma:2026-07` replayed from the committed fixtures (13 findings, 7 planted errors), and benchmark 1 uses the live `suite-skeleton` (see its section), and each is run at least twice, so read the ranges, not the single numbers. Two sources of noise matter. First, the faithfulness judge (sonnet, `evals/judge/rubric.txt`) is itself noisy: re-judging the same explanations moved the supported count by 1 to 2 of 12. Second, tier-2 verified_rate is known to be unstable on identical code. Costs are the `claude-cli` figures priced from `config/pricing.yaml`; the CLI caches prompts, so cost and token counts include its own overhead and are good for ranking, not for budgeting a production bill. The scratch programs that produced these numbers live in `tmp/benchmarks/` (git-ignored, not committed); the run directories named in each section are under `tmp/benchmarks/` on the machine that produced them.

Common setup for the model runs: `. tmp/benchmarks/env.sh` (sets the local Postgres and TEI URLs, the models above and a random `PSEUDONYM_KEY`), Postgres and TEI up, the corpus already ingested (378 chunks: 1 policy, 34 contracts, 6 notes).

## Benchmark 1: workflow only against workflow plus investigator

What was run, on the seeded ERPNext (read-only: no `make erp-reset`, no `make seed`) through the read-only books and evidence MCP servers started from `cmd/mcp-books` and `cmd/mcp-evidence` with throwaway tokens. Suite `suite-skeleton`, both months (`sharma` 2026-09 evaluated, 2026-08 clean control), live, not replayed, so both arms read the same data:

- Workflow only: `tmp/benchmarks/eval run --no-agent --suite suite-skeleton --results-dir tmp/benchmarks/b1/live-noagent-N` (N = 1, 2), scored with `eval score <dir> --pricing config/pricing.yaml`.
- Workflow plus investigator: `tmp/benchmarks/benchbin live -results tmp/benchmarks/b1/live-inv-N` (N = 1 to 4; 4 runs because two were hit by the CLI errors below). `benchbin live` is a scratch program that runs the same live workflow with `agent.LLMInvestigator` (model sonnet, the strong model; tools are the read-only MCP tools through the registry), the redacting provider and the code verifier. It sets no explainer and no retriever, so the added cost is the investigator only.

Each row is one run of both months. Time and cost are the totals of the two month runs.

| Arm | Run | Investigations completed (of 3 unmatched bank lines) | Investigation accuracy vs ground truth | Model cost (USD) | Wall time |
| --- | --- | --- | --- | --- | --- |
| Workflow only | 1 (`live-noagent-1`) | 0 (not run) | n/a | 0 | 606 ms |
| Workflow only | 2 (`live-noagent-2`) | 0 (not run) | n/a | 0 | 324 ms |
| Workflow plus investigator | 1 (`live-inv-1`) | 1 (2 failed with a CLI api_error) | not measurable | 0.1165 | 30.2 s |
| Workflow plus investigator | 2 (`live-inv-2`) | 0 (3 failed with a CLI api_error) | not measurable | 0 | 5.4 s |
| Workflow plus investigator | 3 (`live-inv-3`) | 3 | not measurable | 0.1517 | 36.2 s |
| Workflow plus investigator | 4 (`live-inv-4`) | 3 | not measurable | 0.0810 | 33.8 s |

Cost of the two runs where every investigation completed: 0.081 to 0.152 USD and 34 to 36 s per two-month close, against 0 and under 1 s for workflow only; the 36 s and 34 s include only the investigator calls, since nothing else uses a model in this arm. The failed investigations are `claude cli reported an error (subtype "success", terminal_reason "api_error")`, an error from the CLI side, not a finding; they show the investigator step needs a retry or a clear failed status when the CLI errors.

Investigation accuracy cannot be reported. The suite's ground truth (`evals/scenarios/suite-skeleton/ground_truth/`) names bank lines `BNK-20260922-001` and `BNK-20260929-001` (gateway settlements) and three planted bank charges; the seeded ERPNext and Postgres hold none of them. They hold a different, older small data set for sharma (bank lines `BNK-20260901-001`, `BNK-20260914-007`, `BNK-20260915-003` and `M1-01`), so `eval score` reports 0 of 3 planted charges caught and all five investigation cases `not_surfaced` for the workflow-only arm too, and `Investigation accuracy: not measured` for every run. Re-seeding the suite would fix this, but it wipes ERPNext and was not done. What the investigator did produce on the data that exists, as resolution labels with no ground truth to compare against: `BNK-20260901-001` (rent) resolved `timing_difference` in 2 of 3 completed runs and `unknown` once; `BNK-20260915-003` (a gateway settlement line, hint `gateway`) resolved `unknown` in both runs where it completed; `M1-01` resolved `unknown` in both. The `verified` flag stayed false on all these findings.

**Decision:** keep the deterministic workflow as the default close, and make the investigator an opt-in step for unmatched bank lines: it adds 30 to 36 seconds and roughly 0.08 to 0.15 USD per two-month close for three lines, and I could not show that it resolves them correctly because the seeded data does not match the ground truth. Do not enable it by default until it is scored on a reseeded `suite-skeleton` or `suite-v1`. Main caveats: no accuracy number, a 4-in-12 CLI failure rate in these runs, and a sample of two complete runs.

## Benchmark 2: RAG against the whole policy and contracts in the prompt

What was run, 2 runs of each arm on the same two finding sets:

- RAG arm (the shipped retrieve-and-cite path through the real explainer and code verifier): `tmp/benchmarks/benchbin run -arm rag -model-fast haiku -results tmp/benchmarks/b2/rag-haiku-N` (N = 1, 2; a scratch workflow that replays the suite and retrieves top-3 passages with `Searcher.Search`, 600-character snippets as the evidence server returns them), then `benchbin post -dir tmp/benchmarks/b2/rag-haiku-N -tag judge1|judge2`. Two judge passes per run.
- Whole-document arm: `benchbin post -dir tmp/benchmarks/b2/rag-haiku-N -wholedoc -model haiku -tag whole1-haiku|whole2-haiku`. For each of the 13 findings, one haiku call whose prompt holds the finding, the complete POL-001 manual and all 17 sharma contracts (64 policy chunks and 136 contract chunks; the close notes are left out), pseudonymised like any model call, then the same judge and scoring. 4 generation runs, each judged once.

Citation accuracy is the golden `evals/golden/citations.yaml` cases that apply to this month and whose finding was produced (F02, F03, F04, F06, F07, F08: 6 cases), a case counting right when any cited (doc_id, section) is in its `expect` list, matched by the finding's ground-truth keys. Validity is the share of cited sections that exist in the company's ingested corpus.

| Arm | Run | Citation accuracy (right/6) | Citation validity | Faithfulness (supported/explained) | Input tokens per finding (incl. cache reads) | Cost per finding (USD) |
| --- | --- | --- | --- | --- | --- | --- |
| RAG, haiku | 1 | 2/6 (1 uncited, 3 wrong) | 19/19 | 6/12 in both judge passes | 9,167 | 0.0071 |
| RAG, haiku | 2 | 2/6 (0 uncited, 4 wrong) | 21/21 | 7/12 and 6/12 | 6,222 | 0.0067 |
| Whole documents, haiku | 1a | 6/6 | 25/25 | 5/13 | 52,484 | 0.0062 |
| Whole documents, haiku | 1b | 6/6 | 26/26 | 5/13 | 57,319 | 0.0021 |
| Whole documents, haiku | 2a | 6/6 | 26/26 | 4/13 | 33,089 | 0.0057 |
| Whole documents, haiku | 2b | 6/6 | 26/26 | 6/13 | 40,722 | 0.0018 |

Ranges: citation accuracy 2/6 (RAG) against 6/6 (whole documents); faithfulness 6 to 7 of 12 (50 to 58 percent) against 4 to 6 of 13 (31 to 46 percent), with 0 to 3 unsupported verdicts in the whole-document arm and none in the RAG arm. The RAG arm's code verifier accepted 9 of 13 findings in both runs; the whole-document arm has no verifier, so its verified count is n/a.

Per-finding figures divide the run totals by 13 findings. They are not comparable in the cost column: the whole-document arm's calls hit the CLI's prompt cache hard (hence the low and uneven cost), and the RAG tokens come from stored `llm_calls`, which do not count cache writes, while the whole-document tokens do. The input-token ratio of roughly five to seven times is real in direction, not in size. The whole-document prompt held about 14,000 tokens of documents per finding; each call also carried the CLI's fixed overhead.

**Decision:** keep RAG as the shipped path for cost, auditability and the verifier, but do not claim its citation accuracy is good: with top-3 retrieval on the current query (finding type, supplier, account) it cited an expected section in only 2 of 6 cases, while giving the model everything cited correctly in 6 of 6. The gap is a retrieval-quality finding for CC-1104 (a better query, more passages), not a reason to stuff the prompt, which does not scale past one company's documents and cannot be traced through the retrieval artifact. Main caveat: the whole-document arm saw the finding and evidence references but not the evidence payloads the explainer sees, it has no verifier, and the 6-case, one-month sample is small; I did not measure whether the expected section was inside the RAG arm's retrieved top-3.

## Benchmark 3: retrieval modes

What was run: `. tmp/benchmarks/env.sh && go run ./tmp/benchmarks/recall > tmp/benchmarks/b3/recall.tsv` (a scratch loop copied from `TestIntegrationSearchRecall`, calling `Searcher.SearchMode` with k = 5 for each mode on the 60 queries of `evals/golden/retrieval.yaml`). A query is a hit when any expected (doc_id, section) is in the top 5. 54 queries have expectations; the 6 with `expect: []` (R55 to R60) are tenant-boundary queries, reported separately. Run twice; the two summaries were identical (`b3/recall.tsv`, `b3/recall-2.tsv`). Local Postgres with pgvector, TEI embed and rerank containers.

| Mode | Recall@5 | Policy (18) | Findings (10) | Contracts (20) | Notes (6) | Cross-company hits, all 60 queries |
| --- | --- | --- | --- | --- | --- | --- |
| Vector only | 50/54 = 0.926 | 17 | 8 | 20 | 5 | 0 |
| Hybrid (vector plus full text, RRF) | 50/54 = 0.926 | 17 | 8 | 20 | 5 | 0 |
| Hybrid plus rerank | 51/54 = 0.944 | 16 | 9 | 20 | 6 | 0 |

Misses by key: R12 (policy) is missed in all three modes; R19 (finding type) in all three; R27 (finding type) and R52 (note) in vector and hybrid but found after rerank; R15 (policy) is found by vector and hybrid and lost by rerank. The 6 tenant-boundary queries each returned 5 hits, all from the asking company or the shared policy, so zero cross-company hits in every mode.

**Decision:** keep hybrid plus rerank as the default (it is what `Search` runs), but the evidence for it is thin: one extra hit in 54 over vector only, and hybrid alone added nothing on this set. The tenant boundary is the result that matters and it holds in every mode. Main caveat: with a ceiling near 0.93 on 54 queries, one query is 1.9 points, so the three modes are not separable; a harder query set is needed before tuning (CC-1104) can claim a winner.

## Benchmark 4: fast model against strong model for explanations

What was run: the RAG arm of benchmark 2 with the fast model overridden: `tmp/benchmarks/benchbin run -arm rag -model-fast sonnet -results tmp/benchmarks/b4/rag-sonnet-N` (N = 1, 2) against the haiku runs `tmp/benchmarks/b2/rag-haiku-N`. Both arms still escalate to `LLM_MODEL_STRONG` (sonnet) from the third attempt of a step. Each run judged twice (`-tag judge1|judge2`) with sonnet. 2 runs per model.

| Model | Run | Faithfulness (supported/explained, judge passes) | Verified (code verifier) | Golden citation accuracy | Cost per close (USD) | Wall time per close |
| --- | --- | --- | --- | --- | --- | --- |
| haiku (fast) | 1 | 6/12, 6/12 | 9/13 | 2/6 | 0.0926 | 76.2 s |
| haiku (fast) | 2 | 7/12, 6/12 | 9/13 | 2/6 | 0.0874 | 76.3 s |
| sonnet (strong) | 1 | 4/12, 4/12 | 9/13 | 2/6 | 0.3484 | 58.7 s |
| sonnet (strong) | 2 | 2/12, 3/12 | 9/13 | 2/6 | 0.3220 | 52.2 s |

Ranges: faithfulness 6 to 7 of 12 for haiku, 2 to 4 of 12 for sonnet, with no unsupported verdicts for either (the rest were `partially`); cost 0.087 to 0.093 against 0.322 to 0.348 USD per close, about 3.7 times; time 76 s against 52 to 59 s. For reference, an earlier run on the same month without the retriever (the committed fixtures hold no search results) gave a verified rate of 4/13 (`tmp/benchmarks/b4/haiku-1`), which is the known instability of that metric and not a model effect.

**Decision:** keep haiku as the default explainer model and keep sonnet as the escalation for a third attempt. Sonnet cost about 3.7 times as much and scored no better on faithfulness or on the verifier; the judge rated it lower, which is probably the judge preferring the shorter haiku explanations rather than a real quality gap, and since the judge is the same model family as the strong arm it may not be neutral. Main caveat: judge noise (1 to 2 of 12 between passes) is about as large as the haiku-to-sonnet difference's lower end, only 12 explanations per run, and the wall times differ in the opposite direction from cost for a reason I did not isolate (the logs show retries in both arms); this does not support picking sonnet and does not prove haiku is better.

## What was chosen

Run the deterministic workflow by default and keep the investigator opt-in: benchmark 1 measured its cost and time but not its accuracy, because the seeded data does not match the suite's ground truth. Keep retrieval-augmented explanations on hybrid plus rerank with haiku as the explainer and sonnet only on the third attempt: the tenant boundary held in all 60 queries and every mode, haiku matched or beat sonnet on faithfulness and the verifier at about a quarter of the cost, and stuffing the whole manual and contracts into the prompt is costlier in tokens and less auditable. The one result that argues against the current design is citation accuracy, 2 of 6 for retrieval against 6 of 6 for whole documents, and the retrieval recall numbers show the retriever finds the right section for 94 percent of hand-written queries, so the weakness is the query built from a finding, not the index. That is the first thing for CC-1104 to tune, against these numbers.
