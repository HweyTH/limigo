# ADR-0004 — Report results relative to a measured ceiling, not as bare absolutes

**Status:** Accepted
**Date:** 2026-08-23

## Context

A bare throughput figure is uninterpretable. If Limigo serves 12k req/s, that
number alone cannot distinguish "the rate limiter is slow" from "the environment
tops out at 13k and the rate limiter costs 8%". Those are opposite conclusions and
the raw number supports neither.

The stack had no endpoint that could be load-tested without exercising rule logic:
`POST /v1/check` was the only route on the mux (`cmd/limigo/main.go:99`).

## Decision

Add `GET /healthz`, returning a static 200 and touching no rule logic. Load-testing
it is the **control run**, establishing the environment's ceiling.

Every published results table begins with control rows, and limiter results are
reported as a cost relative to that ceiling.

Three control rows are measured, so each layer's cost is separable:

1. **Direct to a single replica** (in-network) — Go HTTP stack only.
2. **Through Traefik** (in-network) — adds proxy cost.
3. **Host-origin** — adds the Docker Desktop VM network boundary.

`/healthz` is also wired as the compose healthcheck for `limigo`, which previously
had none.

## Alternatives considered

- **Publish absolute numbers only.** Rejected: uninterpretable, as above.
- **A single control row through the full path.** Rejected: it conflates Go's HTTP
  stack, Traefik, and the VM boundary into one figure, so a proxy bottleneck would
  be silently attributed to Limigo.

## Consequences

- Three extra vegeta invocations per results set. Cheap.
- If Traefik or Docker Desktop proves to be the ceiling, that is demonstrable with
  evidence rather than asserted defensively.
- A modest absolute throughput number remains publishable and meaningful, because
  it is framed as a percentage cost against a measured ceiling.
