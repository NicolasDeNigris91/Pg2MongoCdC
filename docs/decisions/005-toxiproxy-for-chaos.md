# ADR-005: Toxiproxy + scripted scenarios for chaos testing

- **Status:** Accepted
- **Date:** 2026-04-20
- **Context pillar:** Resilience & Chaos Testing

## Context

Claiming "the system is resilient" without evidence is the #1 anti-pattern in portfolio projects. We need a chaos harness that is:

1. **Reproducible.** Same command produces the same fault every time.
2. **Scriptable.** Runs in CI, not as a manual ritual.
3. **Laptop-friendly.** No K8s required, no cloud chaos operator.
4. **Granular.** Can inject latency, packet loss, or full connection resets at the network boundary.

## Decision

**Shopify's [Toxiproxy](https://github.com/Shopify/toxiproxy) as the network-layer fault injector, plus bash scripts under `chaos/scenarios/` that each carry their own PASS criterion and integrity check.**

The harness:

```
[k6 load] ──▶ [postgres] ──▶ [connect] ──▶ [toxiproxy] ──▶ [kafka] ──▶ [toxiproxy] ──▶ [sink] ──▶ [mongo]
                                            ▲                              ▲
                                            └────── scenario scripts ──────┘
                                                 inject latency / loss /
                                                 resets / pause container
```

## Why Toxiproxy

1. **TCP-level, application-agnostic.** Works for Kafka, Mongo, HTTP - anything TCP. No application changes to test chaos.
2. **Programmable via HTTP API.** `toxiproxy-cli toxic add -t latency ...` is one line. Scenarios are bash, not framework-specific DSL.
3. **Deterministic.** A latency toxic adds exactly N ms. Not "some latency, sometimes".
4. **Tiny.** ~20MB container, starts in <1s. Runs on a laptop.

## Why scripted scenarios, not a chaos framework

- **Chaos Mesh / Litmus / Chaos Toolkit** all assume Kubernetes or add Python dependencies. They are the right answer at scale; they are wrong for a laptop portfolio demo.
- Bash + `toxiproxy-cli` + `docker` + `curl` + `jq` is enough for five well-chosen scenarios. Every scenario is ~50 lines. A reader can understand the whole chaos harness in 15 minutes.

## The five scenarios (summary)

| # | Scenario | Mechanism | Asserts |
|---|---|---|---|
| 1 | Kill transformer mid-stream | `docker kill -s KILL` | 0 data loss, 0 duplicates after restart |
| 2 | Kafka network partition | Toxiproxy `latency=500ms` + `limit_data` | Consumer lag rises then recovers, no loss |
| 3 | Mongo primary stepdown | `rs.stepDown()` | Sink retries, checkpoint doc monotonic |
| 4 | Postgres WAL pressure | `docker pause connect` for 5 min | Slot survives, heartbeat prevents WAL recycle |
| 5 | Poison event | Inject invalid row | Routed to DLQ, main pipeline unblocked |

Each script:

```
# PASS: pg row count == mongo doc count AND md5(pg rows) == md5(mongo docs)
```

is grepped by `make chaos` to enforce that every scenario *has* a pass criterion. A scenario without a `PASS:` comment fails fast.

## Trade-offs we accept

- **No process-kill fault injection inside containers (OOMKiller, signal storms, clock skew).** Toxiproxy is network-only. We cover container-level faults with `docker kill` / `docker pause`. Kernel-level faults (e.g., `chaos-monkey` style disk fills) are out of scope - if we ever need them, we add a [Pumba](https://github.com/alexei-led/pumba) container.
- **No distributed systems bugs that only manifest at cloud-scale (e.g., cross-AZ clock skew).** Accepted - single-machine chaos catches >90% of bugs in this pipeline class, which is what a portfolio artifact is for.

## Alternatives considered

- **Chaos Mesh.** Requires K8s. Rejected for laptop demo.
- **Gremlin (SaaS).** Paywalled. Rejected.
- **Pumba.** Stronger on container-level chaos, weaker on network-level. Rejected for now - can be added without changing the harness shape.

## Consequences

- Toxiproxy runs in `docker-compose.chaos.yml` as an overlay, not in the default `docker-compose.yml`. The default stack stays a straight-line diagram.
- Every chaos script is expected to be self-verifying (runs `verify-integrity.sh` at the end) so the binary PASS/FAIL from `make chaos` is meaningful in CI.
