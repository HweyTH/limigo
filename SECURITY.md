# Security

Limigo sits in the request path in front of whatever it protects, so its
failure modes are availability failures for that service. This document says
what counts as a vulnerability here, what does not, and how to report one.

## Supported versions

Limigo is pre-1.0. Only the latest tagged release receives fixes. There is no
backport branch.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: **Security → Report a
vulnerability** on this repository. Do not open a public issue.

Include the rule configuration, the request sequence, and the observed
behaviour. A reproduction against the Docker Compose stack in this repository
is ideal, since it is the same image and the same Lua scripts a deployment
would run.

Expect an acknowledgement within a week. This is a single-maintainer project;
there is no bounty.

## What is in scope

Anything that makes the limiter admit or deny traffic other than what its
configured rules say, or that lets traffic through it degrade the service
behind it:

- **Limit bypass.** A request sequence, header, or key shape that gets more
  requests admitted than a rule's limit allows — beyond the bounded overshoot
  that `local_cache: true` documents and measures in the README.
- **Cross-rule or cross-key interference.** One key's traffic changing the
  decision for another key, or one rule's state leaking into another's.
- **Unbounded memory growth.** Every limiter keeps per-key state, and the
  node-local caches behind `local_cache: true` keep it in-process. A key
  cardinality pattern that grows a node's memory without bound is a
  vulnerability, because the attacker chooses the keys.
- **Denial of service through the limiter.** Request shapes that hold
  connections, goroutines, or Redis round-trips long enough to starve
  legitimate traffic — the limiter becoming the outage rather than preventing
  it.
- **Fail-closed violations.** When Redis is unreachable, `/v1/check` must
  return `503` with `allowed: false`. Any path that returns `allowed: true`
  during a store failure is a vulnerability.

## What is not in scope

- **The `bench/` harness.** It is a measurement tool run on a developer
  machine against a throwaway stack. It is not a deployment target.
- **The Docker Compose development stack.** `docker-compose.yml` is for
  running Limigo locally and watching it. By design it publishes Prometheus
  (`:9090`) and Grafana (`:3000`, anonymous viewer, `admin`/`admin`) with no
  authentication, runs Redis with no password on the Compose network, and
  mounts the Docker socket read-only into Traefik and Prometheus for replica
  discovery. None of that is a finding; none of it should be exposed beyond
  a local machine.
- **Redis itself.** Limigo trusts its Redis. Securing Redis — authentication,
  TLS, network isolation — is the deployment's responsibility, and Limigo's
  Lua scripts assume the store is not adversarial.
- **Rule configuration as an attack surface.** Whoever can write the config
  file can set any limit. The hot-reload path validates the file; it does not
  authenticate the author.
