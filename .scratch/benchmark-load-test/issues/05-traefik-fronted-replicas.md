# 05 — Traefik-fronted, scalable Limigo replicas

**What to build:** The compose stack runs any number of Limigo nodes behind a
single entry point. `docker compose up --scale limigo=3` starts three healthy
replicas, all reachable through one address, with requests distributed across them.

Today the stack cannot run more than one node: the `limigo` service binds fixed
host ports, and two containers cannot claim the same host port, so scaling fails
outright. That blocks every multi-node measurement — and multi-node is the whole
point, since the project's central claim is about shared state staying correct
across nodes, which is unobservable with one node (ADR-0003).

Remove the fixed host port bindings and put Traefik in front as the single entry
point, load-balancing across replicas.

Prometheus must scrape **every** replica, not just one — otherwise the metrics
silently describe a fraction of the cluster and every dashboard becomes wrong in a
way that is hard to notice.

`docker compose up` must remain a single command for the quickstart.

**Blocked by:** 02 — Add `/healthz` control endpoint (used as the service healthcheck). Done.

**Status:** done — all checklist items verified live

Code changes: `docker-compose.yml` (traefik added, fixed host ports removed
from `limigo`, `prometheus` reads docker.sock and runs as root),
`prometheus/prometheus.yml` (`docker_sd_configs` replacing the static
target), `README.md` updated.

Verified live after the host restart cleared the earlier Docker Desktop
storage corruption (`docker compose up -d --build --scale limigo=3`):

- [x] `docker compose up --scale limigo=3` starts three replicas, all reporting healthy
- [x] All replicas are reachable through one address, and repeated requests demonstrably reach more than one
- [x] Fixed host port bindings are gone from the `limigo` service
- [x] Prometheus discovers and scrapes every replica, verified with more than one node running
- [x] Grafana dashboards still render with multiple replicas running — queried panel 1's exact expr (`sum by (result) (rate(limigo_requests_total[1m]))`) through Grafana's own datasource proxy while traffic was live; returned a nonzero `allowed` series, not just an empty provisioned shell
- [x] `docker compose up` alone still works for the single-node quickstart
- [x] Individual replicas remain reachable from inside the compose network for direct measurement — hit `limigo-limigo-{1,2,3}:9091/metrics` directly from a throwaway container on `limigo_default` (200 on each) and confirmed each replica's own `limigo_requests_total` counter incremented independently (11/10/10 after 30 requests through Traefik), rather than inferring it from Prometheus's scrape
