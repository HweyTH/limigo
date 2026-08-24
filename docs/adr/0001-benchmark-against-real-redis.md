# ADR-0001 — Benchmark against real Redis, never an in-memory fake

**Status:** Accepted
**Date:** 2026-08-23

## Context

Limigo's correctness claim rests on executing counter logic as Lua *inside*
Redis, so that check-and-increment is atomic. Benchmarks need a Redis to talk to.
The obvious cheap option is `miniredis`, an in-process Go fake: no Docker, fast
startup, easy CI.

The existing test suite already uses `testcontainers-go/modules/redis` and spins
up a real Redis (`internal/store/redis_store_test.go`).

## Decision

Redis-backed benchmarks use real Redis via testcontainers, matching the existing
test suite. They are gated behind a `//go:build bench` tag so `go test ./...`
stays fast and Docker-free.

Pure-algorithm benchmarks — direct `Allow()` calls with no store — carry no build
tag and always run.

## Alternatives considered

- **miniredis.** Rejected. It does not execute Lua the way real Redis does. A
  benchmark against it would measure Go function-call overhead and report it as
  Lua execution cost — precisely the wrong number, and wrong in the flattering
  direction.
- **A shared long-lived Redis on the developer's machine.** Rejected: state leaks
  between runs, and results depend on an unversioned local install.

## Consequences

- Redis-backed benchmarks require Docker and are slower.
- The `bench` tag must be passed explicitly: `go test -tags bench -bench . ./...`.
- Benchmark numbers include container-boundary network cost. This is accepted:
  the deployed system also talks to Redis over a socket, so the measurement is
  representative rather than idealised.
