# ADR 0005: A deterministic workflow with a bounded agent, not a free agent loop

- Status: accepted
- Date: 2026-10-14
- Decides: how a close run is orchestrated and where a model may choose its own steps (CC-703, CC-706, CC-709)

## Context

The steps of a close are known in advance: check, retrieve the relevant documents, explain, verify, investigate what is left, report. A run must be repeatable and must resume after a crash without redoing finished work or duplicating findings. Every finding must be rebuildable from stored artifacts (CC-709). Cost and time must be bounded per run.

One class of problem doesn't fit a fixed path. An unmatched bank line the rules can't classify, such as a payment-gateway settlement paid out net of fees, needs a few exploratory lookups whose order isn't known beforehand.

## Options

1. **A free agent loop.** One model holds all the tools, plans the close and decides when it is done. It is flexible. It lost because cost and latency would be unbounded and the step sequence would differ between runs, so a resume couldn't skip "the same" finished step. It would also put retrieved document text and tool access in one context, which is the setting prompt injection needs. Scoring it against ground truth would measure the planner's variance as much as the checks.
2. **A fixed pipeline with no agent at all.** It is fully deterministic. It lost because the lines that need an open-ended path would stay unresolved and go to a human with no work done on them.
3. **A fixed state machine, with one bounded tool-using step for unmatched bank lines (chosen).**

## Decision

- The workflow (`internal/agent/workflow.go`) moves a run through `queued → preflight → checking → retrieving → explaining → verifying → investigating → synthesizing → done | partial | failed`. Each step is a `run_steps` row keyed by (run, kind, subject), so `agent resume <run_id>` skips what is done. Steps after checking take their subjects from the stored findings, so finding IDs stay stable across a resume.
- Only explaining, verifying (the model's answer, checked in code) and investigating involve a model. The explainer can call nothing but `emit_explanation`.
- The investigator (`internal/agent/investigate.go`) runs per `unmatched_bank_line` finding. It is offered only the nine read tools and `emit_resolution`, never `search_documents` and never the admin tool. It is capped at 8 tool calls, 90 seconds and 30,000 tokens per finding, and tool results are projected with free text dropped or fenced. If it breaches a limit or makes a refused call, the finding ends `needs_review`, and its proposal is dropped. Its answer goes through a verify step like an explanation.
- The investigator is opt-in. Benchmark 1 measured its cost but couldn't measure its accuracy, so it isn't enabled by default ([docs/benchmarks.md](../docs/benchmarks.md)).

## Consequences

- **Resume and audit work.** At Gate B, a `kill -9` mid-run resumed to `done` with zero duplicate steps, and `cmd/audit rebuild` reproduced 6 of 6 findings from stored artifacts (`tasks/gate-b-review.md`).
- **Bounded cost.** On suite-skeleton a close run costs about 0.003 USD and its p95 duration is about 9 s with the explainer on (CC-1104). In benchmark 1, the investigator added 0.08 to 0.15 USD and 34 to 36 s per two-month close for three lines.
- **The investigator isn't in production yet.** No binary sets it (`cmd/agent` and `cmd/eval` record a skipped investigate step). Its accuracy is unmeasured, and 4 of 12 investigations in benchmark 1 hit a `claude -p` `api_error`. It needs a retry or a clear failed status before it is turned on.
- **Less flexible.** A problem outside the checks and outside unmatched bank lines gets no model attention at all. That is the price of the bound.
- **What would change this decision:** a scored investigator on a reseeded suite showing that it resolves lines correctly. That would justify enabling it by default. A widening of its scope would need its own allowlist, limits and verifier, not a move to a free loop.
