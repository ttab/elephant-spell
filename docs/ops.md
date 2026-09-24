# elephant-spell — operations

For somebody holding a pager or triaging a report that spellchecking is wrong. It assumes no knowledge of the code.

| Document | What it settles |
|---|---|
| [`../README.md`](../README.md) | What the repository holds, how to build and run it, and what every configuration flag does. |
| [`architecture.md`](architecture.md) | How the service is built, and why. |
| **`ops.md`** (this document) | Dependencies, deployment shape, bootstrap order, and the failure modes with the signal for each. |
| [`observability.md`](observability.md) | Every metric the service exports and what a change in it means. |
| [`../CONTEXT.md`](../CONTEXT.md) | What the words mean. |
| [`adr/`](adr/) | Why a decision went the way it did, and what was reversed. |

This document does not explain how the service is built — that is [`architecture.md`](architecture.md) — and it names metrics without defining them, which [`observability.md`](observability.md) does.

> **The failure-mode catalogue below is derived from the code, not from incidents.** No incident history has been written into this repository yet. Where a section says what a failure looks like, that is what the code implies it looks like; none of it is a reading taken during a real one. The numbers that would date and calibrate these claims are missing, and the first real incident should be written in here with them.

## What the service is

One process with two halves that fail independently in principle and not in practice.

- **The serving half**: three Twirp services and a web UI, answering out of in-memory tries.
- **The following half**: a `LISTEN` subscriber, an eventlog consumer and a pruner, keeping those tries in line with Postgres.

**All four tasks are `Required`, so the death of any one takes the process down.** There is deliberately no mode in which the service keeps answering spellchecks after it has stopped following the dictionary — a replica serving a silently frozen dictionary is worse than a replica that is gone, because nothing downstream can tell.

## Components

| Repository | What it is to us |
|---|---|
| `ttab/elephant-spell` | This service. |
| `ttab/elephant-platform` | Where it is deployed from, and where its configuration and secrets live. |
| `ttab/elephant-api` | The `elephant.spell.*` protobuf declarations its clients are generated from. |
| `ttab/howdah` | The web UI framework: OIDC login, session cookies, the page mux. |
| `ttab/elephantine` | The API server, auth, job locks and the LISTEN/NOTIFY plumbing. |

## Deployment shape

| | |
|---|---|
| Role | Stateless replicas, all identical. |
| Configuration | Environment, from elephant-platform. |
| Runs | The HTTP surface, plus the subscriber and eventlog consumer on every replica. |

**Every replica holds the entire custom dictionary in memory and follows the eventlog independently.** Replicas scale read throughput and nothing else: they do not shard the dictionary, and adding one adds a full copy of it to the cluster's memory footprint and one more eventlog consumer to the database.

Exactly one replica holds the `eventlog-prune` job lock at a time. That is the only asymmetry.

## Runtime dependencies

| Dependency | Needed for | What happens without it |
|---|---|---|
| PostgreSQL (direct, `CONN_STRING`) | `LISTEN/NOTIFY`, the eventlog, all reads and writes | The service will not start. Once running, a lost connection stops dictionary updates; the serving half keeps answering from memory until the process exits. |
| PgBouncer (`BOUNCER_CONN_STRING`) | General queries, optionally | Nothing — queries fall back to the direct pool. It is an optimisation, not a requirement. |
| OIDC provider | The web UI's login, and token validation for RPCs | The UI cannot log anyone in. **The service will not start**: `oidc-provider`, `client-id`, `client-secret` and `callback-url` are all required flags and provider discovery runs during startup. |
| `COOKIE_KEY_*` | Sealing the session cookie | **The service will not start.** howdah has no unsealed-cookie mode. |
| Writable temp space | Unpacking the embedded hunspell dictionaries at startup | The service will not start. Needed during construction only. |

**Truly required: Postgres, the OIDC provider, at least one cookie key, and somewhere to write a temp directory.** Everything else degrades rather than blocks.

## Endpoints and ports

| Port | Default | What is on it |
|---|---|---|
| API | 1080 | The three Twirp services, the web UI, `/health/alive`. |
| Profiling | 1081 | `/metrics` and pprof. **Never expose this.** |
| TLS | 1443 | The same API surface, when `TLS_CERT_PATH` and `TLS_KEY_PATH` are set. |

## Data flows

### 1. A dictionary edit

```
  editor in the admin UI          or  a client calling SetEntry
        │                                      │
        └──────────────┬───────────────────────┘
                       ▼
              spell_write scope checked
                       │
                  ┌────▼──── one transaction ────────────┐
                  │ write entry/rule row                 │
                  │ LOCK TABLE eventlog EXCLUSIVE        │
                  │ INSERT eventlog → id                 │
                  │ pg_notify('eventlog', id)            │
                  └────┬─────────────────────────────────┘
                       ▼ COMMIT
                 every replica's subscriber wakes
                       ▼
                 drainEventlog: read past cursor,
                 re-read each row, update the tries
```

The operational weight is in the exclusive table lock: it serialises every dictionary write across the fleet, for the duration of the writing transaction. That is invisible at editorial write rates and would not be under a bulk load — a large import driven through `SetEntry` rather than through the importer would queue behind itself.

The notification carries an id that nobody reads as data. **Losing a notification costs up to one minute of staleness and never costs correctness**, because the consumer reads everything past its own cursor and the fallback poll runs every minute regardless.

### 2. A spellcheck

```
  caller → Check/Text → per-language *Spellcheck (read lock)
                              │
                              ├── phrase tries: sliding 3-word window
                              ├── mistake tries
                              ├── pattern rules (compiled RE2)
                              └── hunspell            ← skipped if custom_only
                              ▼
                        longest match wins,
                        context guards applied
```

No database access is on this path. **A spellcheck is served entirely from memory**, which is why the serving half survives a database outage and why a replica's answers are only as current as its last successful drain.

### 3. Pruning

```
  the replica holding the eventlog-prune lock
        │  every 15 minutes
        ▼
  DELETE FROM eventlog WHERE created < now() - 1 hour
```

## Single-leader work

| Lock | Does | When nobody holds it |
|---|---|---|
| `eventlog-prune` | Deletes events older than an hour, every 15 minutes. | The eventlog grows without bound. Nothing breaks quickly — reads are cursor-based and indexed — but the table and its bloat grow until somebody notices. |

## Where state lives

| Store | Holds | Authoritative? |
|---|---|---|
| `entry`, `rule` tables | The custom dictionary and the pattern rules. | **Yes.** Everything else is derived. |
| `eventlog` table | The change feed, one hour deep. | No. A transport, not a record. |
| In-memory tries, per replica | The working copy that answers every check. | No. Rebuilt from `entry` and `rule` on start. |
| Embedded hunspell dictionaries | The base language dictionaries. | Yes, and they ship in the binary — changing one is a release. |

**The eventlog is not a source of truth and nothing may be recovered from it.** It is pruned to an hour precisely because a restarting replica reloads full state and resumes from the newest id; the window only has to outlast lag on a running replica. This is a different thing from elephant-repository's eventlog, which is the platform's durable record — see [`../CONTEXT.md`](../CONTEXT.md).

## Bootstrap order

1. **Migrations, before the deploy.** `go run ./cmd/setup db migrate` in elephant-platform. The service never migrates itself.
2. **`COOKIE_KEY_1` in the environment.** Without it the process exits at startup. If this is a first deploy of a version on howdah v0.5.0 or later, provision it *before* rolling, not after the first pod fails.
3. **The OIDC client must exist** and `callback-url` must be registered as a redirect URI for it. Provider discovery happens during startup, so a wrong `oidc-provider` is a startup failure rather than a login failure.
4. **Then the replicas.** Each preloads the full dictionary before serving correctly; see the readiness gap below.

Out of order, the common one is (2): a rollout where every pod crashloops on `no cookie keys configured, expected at least one COOKIE_KEY_* environment variable`.

## Failure modes

### Spellchecks come back missing recent entries

**What it looks like:** an editor adds a word, and it is still flagged — but only sometimes, or only for some users. Refreshing helps at random.

**The signal:** `pg_fanout_eventlog_poll_saved_streak` non-zero on some replicas and not others. If it is zero everywhere and the problem persists, look for `drain eventlog on notification` or `poll eventlog` at error level in the logs.

**What to do:** a non-zero streak that sawtooths is the recovery bouncing the subscriber and working; leave it, but find out why `LISTEN` is dying. A flat non-zero plateau means recovery is not clearing it — restart the affected replicas, then check whether `CONN_STRING` is pointed at PgBouncer, which breaks notifications permanently.

**Worth knowing:** because each replica follows the log independently, this is normally a *subset* of replicas. A load balancer spreading requests is what makes it look intermittent to one user.

### Nothing sees dictionary changes at all, on any replica

**The signal:** `pg_fanout_eventlog_poll_saved_total` climbing steadily on every replica since the deploy, with the streak non-zero across the board.

**What to do:** check `CONN_STRING`. `LISTEN` does not survive PgBouncer's transaction pooling, and a deployment that points the direct pool at the bouncer starts cleanly, serves reads, and converges only on the one-minute fallback poll. `BOUNCER_CONN_STRING` is the one that may point at the bouncer.

### The service crashloops on startup

**The signal:** pods restarting, no `/health/alive`, and one of these in the logs:

| Log line | Cause |
|---|---|
| `no cookie keys configured…` | `COOKIE_KEY_1` is missing. |
| `read cookie keyring: …` | A key is malformed — the format is `<RFC 3339>_<base64 of 32 bytes>`. |
| `create OIDC provider` | `oidc-provider` is wrong or the provider is unreachable. |
| `pubsub database: …` | `CONN_STRING` is wrong or Postgres is down. |
| `bouncer database: …` | `BOUNCER_CONN_STRING` is wrong or the bouncer is down. |
| `create dictionary directory` | No writable temp space. |

### The eventlog is growing without bound

**The signal:** `sum(pg_job_lock_held{name="eventlog-prune"})` at 0, or `pg_job_lock_restarts_total` climbing.

**What to do:** a sum of 0 means no replica is holding the lock — usually because every replica is unhealthy for some other reason, so look there first. A climbing restart count means the prune itself is failing; the error is logged as `prune eventlog`.

### Requests that used to be refused with 403 are now refused with 401

**Not a failure.** elephantine v0.29.0 answers an unparseable or invalid token `unauthenticated` (401) where it previously answered `permission_denied` (403). `permission_denied` now means a caller we identified that lacks `spell_write`. Anything keyed on 403 — an ingress rule, a dashboard panel, a client's retry logic — needs updating, not the service.

### A replica takes traffic before its dictionary is loaded

**The signal:** none. This is the gap, not the symptom.

Nothing is registered with `AddReadyFunction` or `AddOptionalReadyFunction`, so `/health/alive` answers as soon as the HTTP listener is up — while the replica may still be paging through the preload. During a rollout of a large dictionary this is a window in which a fresh replica answers checks against a partial dictionary and reports itself healthy.

**What to do:** nothing operationally; this needs a readiness check. Until then, roll slowly enough that a replica has finished preloading before the next is replaced.

## What to watch, in order

1. **`pg_fanout_eventlog_poll_saved_streak`** — the only signal that a replica's dictionary is going stale, and the failure most likely to be reported as "spellcheck is wrong" rather than as an outage.
2. **`rpc_responses_total{code="internal"}`** — the service failing on the request path. Everything else here is about freshness; this is about working at all.
3. **`sum(pg_job_lock_held{name="eventlog-prune"})`** — should be exactly 1. Zero is slow-burning and nothing else will tell you.
4. **`task_restarts_total`** and pod restarts together — every task is `Required`, so a divergence between the two is a crash that was not a clean task exit.
5. **`go_memstats_heap_inuse_bytes`** — a proxy for dictionary size, watched for step changes rather than levels, because nothing exports the dictionary size directly.

## Common operations

**Check whether a replica is current.** There is no metric for it. Exec into the pod's logs and look for the most recent `pruned eventlog` or drain error; otherwise compare a known-recent entry through `GetEntry` (which reads the database) against `Check/Text` (which reads memory). A disagreement is a stale replica.

**Roll a cookie key.** Add `COOKIE_KEY_2` with a use-after timestamp in the future, deploy, wait for the timestamp to pass, then remove the old key no sooner than the maximum session age after that. Both keys open while only the newest eligible one seals. The README's [Cookie keys](../README.md#cookie-keys) has the format.

**Force a full dictionary reload on a replica.** Restart it. There is no other mechanism — the preload only runs at startup.

**Run the migrations.** `go run ./cmd/setup db migrate` from elephant-platform, before the deploy.

## Security

**Inbound.** Every RPC requires a valid token; there is no anonymous surface. `spell_write` gates every mutation of a dictionary entry or rule. Reads and spellchecks need only a valid token.

**The web UI** authenticates through OIDC and calls the service's own RPC implementations in-process, bridging the browser session into the `AuthInfo` the handlers check. The scope check is the same one the RPCs use, so the UI cannot write something an RPC caller could not.

**Cross-site writes to UI routes are refused** and UI pages cannot be framed; both come from howdah's `PageMux` and neither is configurable. A route that must accept a cross-origin post belongs on the bare mux instead.

**Outbound.** The service makes no upstream calls on behalf of a caller. The only outbound traffic is to the OIDC provider, for discovery at startup and for the token exchange during a login — both on its own behalf, neither influenced by a caller.

**Secrets:** `CLIENT_SECRET`, `COOKIE_KEY_*`, and the database connection strings. All come from the environment, via elephant-platform.

**Not a write path:** `Check/Text` and `Check/Suggestions` touch no table and hold no lock.

## Not in place yet

- **No readiness check**, so a preloading replica reports itself alive.
- **No metrics of this service's own** — no eventlog lag, no dictionary size, no spellcheck counters. See [observability.md](observability.md#what-is-missing).
- **No incident history.** The catalogue above is read off the code. The first real incident should be written in with its numbers.
