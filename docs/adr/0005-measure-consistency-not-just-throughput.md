# ADR-0005 — Measure the consistency trade-off; make it the headline result

**Status:** Accepted
**Date:** 2026-08-23

## Context

Limigo offers `local_cache: true`, which lets a node absorb bursts in-process and
reconcile with Redis on a ticker (`--cache-flush-interval`, default 10ms). The
consequence is stated plainly in the docs: across N nodes, the configured limit
can be exceeded before any flush lands.

That consequence was asserted in prose and measured nowhere. The size of the
overshoot, whether it is bounded, and how it responds to the flush-interval knob
were all unknown.

Meanwhile the planned deliverable was throughput numbers — which every rate
limiter README already has.

## Decision

The scaling test asserts **correctness**, not only throughput, and the resulting
consistency measurement is the headline result of the benchmarking effort —
ranked above any throughput figure.

Method: issue a fixed, known number of requests against a low limit across 1, 2,
and 3 nodes; count actual allowed responses; publish the error as a percentage of
the configured limit ("overshoot"), for both configurations:

- `local_cache: false` — expected ~0%, demonstrating that Lua atomicity holds
  under genuine cross-node concurrency.
- `local_cache: true` — expected non-zero and bounded, growing with node count and
  with flush interval.

Then vary `--cache-flush-interval` to produce an accuracy/latency curve, showing
what each increment of accuracy costs in latency.

`config.example.yaml` already contains the controlled pair this needs:
`burst-tier-token-bucket` (capacity 1000, rate 200, no cache) against
`cached-tier-token-bucket` (identical parameters, `local_cache: true`). Same
algorithm, same limits, one variable.

## Alternatives considered

- **Throughput only, consistency argued from the Lua source.** Rejected: it leaves
  the project's most distinctive engineering decision unverified, and an unmeasured
  trade-off is indistinguishable from an unnoticed bug.

## Consequences

- A number will be published showing Limigo exceeding its own configured limits
  under `local_cache: true`. This is the intended, documented behaviour and is
  presented as a quantified trade-off with a tunable knob — not as a defect.
- If overshoot turns out to be unbounded or to grow super-linearly with node
  count, that is a genuine design finding and blocks the local-cache feature from
  being recommended as a default.
- The correctness harness must count responses exactly, so the load generator
  needs deterministic request counts — an attack-mode run, not an open-ended
  duration-based one.
