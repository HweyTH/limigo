# ADR-0002 — Publish measured numbers with provenance; retire the unverified 50k target

**Status:** Accepted
**Date:** 2026-08-23

## Context

`CLAUDE.md` stated a performance target — "50k req/s across 3 nodes, p99 < 5ms" —
and listed it among the things the README must contain. No harness existed and no
run had ever produced a number. The figure was an aspiration that read, in place,
as a result.

Separately, the target is unlikely to be reachable on the intended development
environment. On Docker Desktop for macOS the VM network boundary becomes the
bottleneck well before Limigo's own code does.

## Decision

Publish only measured numbers, each with stated hardware and an exact reproducible
command. The unverified 50k / p99 < 5ms figure is removed from `CLAUDE.md` and
replaced with a pointer to measured results.

The "3 nodes" framing is replaced by a **scaling curve** at 1, 2, and 3 replicas.
The claim being made is "throughput scales approximately linearly with node count
while shared state stays correct" — not a single absolute throughput number.

If measured throughput lands far below 50k, that is published as-is, together with
evidence identifying the bottleneck (see ADR-0004 on control runs).

## Alternatives considered

- **Treat 50k as a hard target and tune until it's hit.** Rejected: it inverts the
  process, letting a number invented before any measurement dictate what gets
  reported.
- **Publish real numbers alongside 50k as a labelled aspiration.** Rejected, and
  this was the tempting one. An aspirational figure sitting next to measured ones
  reads as padding and undermines the credibility of the real numbers.

## Consequences

- The README may carry a throughput number well under 50k. Accepted.
- A scaling curve plus a control-relative cost is a stronger and more defensible
  claim than a round absolute number with no provenance.
- Every results table needs a hardware line and a repro command, which is more
  work per published figure.
