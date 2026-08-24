# ADR-0003 — Front the compose stack with Traefik and scale Limigo as replicas

**Status:** Accepted
**Date:** 2026-08-23

## Context

`docker-compose.yml` defined a single `limigo` service with host ports bound
`8080:8080` and `9091:9091`. Fixed host port bindings make `docker compose up
--scale limigo=3` fail — two containers cannot claim the same host port — so the
stack could not run multiple nodes at all.

Multi-node is not optional here: the project's central claim is about shared state
staying correct across nodes, which is unobservable with one node.

## Decision

Remove the fixed host port bindings from `limigo` and place Traefik in front as
the single entry point, load-balancing across N replicas. Node count is varied by
scaling that one service.

## Alternatives considered

- **Three explicitly-named services `limigo-1/2/3` on distinct host ports.**
  Rejected: triplicates configuration, and the load generator has to implement
  round-robin itself, making the generator part of the topology under test.
- **Keep one node and argue correctness from the Lua scripts.** Rejected — that is
  the assertion the measurement exists to replace.

## Consequences

- Traefik sits on the request path and costs something. That cost is isolated
  rather than absorbed: ADR-0004 requires a direct-to-replica control row
  alongside the through-Traefik one.
- Individual replicas are no longer addressable from the host by fixed port.
  Direct-to-replica measurements run from inside the compose network.
- `docker compose up` remains a single command for the quickstart.
