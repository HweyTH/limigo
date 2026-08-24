# ADR-0006 — Use vegeta as the load generator

**Status:** Accepted
**Date:** 2026-08-23

## Context

`CLAUDE.md` allowed either vegeta or k6. The choice carries mild lock-in: targets
files, output format, and the CI shape all follow from it.

## Decision

vegeta.

## Alternatives considered

- **k6.** Richer scenario modelling — ramping virtual users, stages, built-in
  thresholds — and nicer output. Rejected because the measurements here are
  steady-rate throughput and percentile claims, which need no ramp modelling, and
  it introduces a JavaScript runtime into an otherwise pure-Go project.

## Consequences

- vegeta is a single static Go binary: trivial to install, trivial to containerise
  for in-network runs, no runtime dependency to explain in the quickstart.
- `vegeta attack -targets` plus `vegeta report -type=hdrplot` produces
  histogram output that drops into a README table with little massaging.
- Deterministic request counts for the correctness runs (ADR-0005) come from
  `-rate` and `-duration`, or from a bounded targets file.
- If multi-stage ramp scenarios are ever needed, this decision must be revisited;
  vegeta does not model them well.
