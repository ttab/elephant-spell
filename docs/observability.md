# Observability

Every metric this service exposes and what a *change* in it means. The definitions are in the names; what follows is which direction is bad, what is routinely non-zero, and what each number should be read against. For what to do about a reading, see [`ops.md`](ops.md).

| Document | What it settles |
|---|---|
| [`../README.md`](../README.md) | What the repository holds, how to build and run it, and what every configuration flag does. |
| [`architecture.md`](architecture.md) | How the service is built, and why. |
| [`ops.md`](ops.md) | Dependencies, deployment shape, bootstrap order, and the failure modes with the signal for each. |
| **`observability.md`** (this document) | What each number means. |
| [`../CONTEXT.md`](../CONTEXT.md) | What the words mean. |
| [`adr/`](adr/) | Why a decision went the way it did, and what was reversed. |

This document does not tell you what to do when a number is wrong — that is [`ops.md`](ops.md)'s failure-mode catalogue — and it does not describe the subsystems the metrics come from, which is [`architecture.md`](architecture.md).

## Where the metrics come from

`/metrics` is on the profiling listener, port **1081** by default, not on the API port.

**Almost every series this service exposes is registered by a library, not by this repository.** `main` passes `prometheus.DefaultRegisterer` down to `internal`, whose `NewMetrics` registers the service's only collector of its own, the hunspell handle pools; `main` registers elephantine's pool statistics collector for each database pool it opens. That is a gap rather than a design choice; see [What is missing](#what-is-missing) at the end, and read the rest of this document knowing that apart from the handle pools nothing here measures spellchecking.

| Source | Series |
|---|---|
| `internal.NewMetrics`, read from each language's `hunspell.Checker` at scrape time | `spell_hunspell_*` |
| `elephantine` service options: the Twirp hooks and the Connect interceptor, which share one set of collectors | `rpc_*` |
| `elephantine.ErrGroup` | `task_restarts_total` |
| `elephantine/pg` FanOut recovery | `pg_fanout_eventlog_*` |
| `elephantine/pg/joblock` | `pg_job_lock_*` |
| `elephantine/pg` pool statistics, registered in `main` | `pgxpool_*` |
| `elephantine` health | `health_check_up` |
| Go runtime and process collectors | `go_*`, `process_*` |

## RPC surface

**The pair to watch is `rpc_responses_total` split by code against `rpc_requests_total`.** A rising `internal` rate is the service failing; a rising `permission_denied` rate is a caller that lost a scope, which looks identical from a dashboard that only counts errors.

- `rpc_requests_total{service,method}` — call volume. The baseline is editorial traffic against `Check/Text`, so it tracks the working day; a flat line through the morning is a caller that has stopped, not a quiet service.
- `rpc_duration_seconds{service,method}` — `Check/Text` carries the real work. Latency here scales with the size of the text and the number of loaded entries, so a step change with no deploy means the dictionary grew, not that Postgres slowed down. `Suggestions` is slower per call by nature — hunspell suggestion generation is the expensive path.
- `rpc_responses_total{service,method,code}` — the error breakdown. `invalid_argument` is routine: it is what a malformed request from the UI or a client looks like. `unauthenticated` is routine at low rates (expired tokens) and a problem in a step; **since elephantine v0.29.0 a bad token is `unauthenticated`, where it used to be `permission_denied`** — a panel keyed on the old code reads zero.
- `rpc_protocol_responses_total{service,method,protocol,code,client_id}` — the same breakdown with the protocol and the calling client. Both mounts are served, so **this is the series that says how far the migration off Twirp has got**: `protocol="twirp"` falling to zero for a method is what says that method's Twirp mount can be removed, and `client_id` — the token's client id claim, empty for an anonymous caller — names the applications that still have to move. The `code` label is the error breakdown `rpc_responses_total` cannot give, since the two stacks disagree on the HTTP status for some codes.

## Hunspell handle pools

Every series carries a `language` label, one value per embedded dictionary (`sv-se`, `fi-fi`, ...). Each language has its own pool of hunspell handles, and every call into hunspell borrows one; see [the hunspell handle pool](architecture.md#the-hunspell-handle-pool).

**The one to watch is `spell_hunspell_handle_waits_total` against `spell_hunspell_handles`.** A wait rate on a language whose pool is at `HUNSPELL_HANDLES` is checks queueing for hunspell, and the time they queue is added to `Check/Text` and `Check/Suggestions` latency.

- `spell_hunspell_handles` — handles loaded. It starts at 1 and steps up, to at most `HUNSPELL_HANDLES` (4 by default), when checks find every handle busy; it never goes down before a restart. **A language above 1 has seen concurrent load**, and each step is 4–41 MiB more resident memory, depending on the language.
- `spell_hunspell_handles_in_use` — handles lent to calls right now. Sitting at `spell_hunspell_handles` means every handle is busy. It does not count handles held by a dictionary update, which briefly takes them all.
- `spell_hunspell_handle_waits_total` — calls that found every handle busy. Read it as a rate. **Zero is the normal value at low traffic**; short bursts while the pool grows are expected. A sustained rate at the maximum pool size means raising `HUNSPELL_HANDLES` if the replica has CPU to spare, and adding replicas if it does not.
- `spell_hunspell_handle_wait_seconds_total` — the total time spent waiting. Divided by the waits rate it is the average wait per waiting call.
- `spell_hunspell_handle_canceled_waits_total` — calls whose request ended before a handle came free. A rate here is callers timing out on the queue.
- `spell_hunspell_handle_create_failures_total` — failed attempts to load another handle. **It should never move**: the dictionaries are embedded, so a failure is the temp directory being full or unwritable, and the language is stuck below its maximum pool size.

## Eventlog fanout

**The signal that the dictionary is going stale on a replica is `pg_fanout_eventlog_poll_saved_streak`, and it is the most important number in this document.**

- `pg_fanout_eventlog_poll_saved_streak` — consecutive fallback-poll drains that found work with no notification in between. **Zero is the healthy value and any sustained non-zero reading means `LISTEN` is delivering nothing on that replica**: the dictionary is still converging, but on the one-minute poll rather than in real time. It counts drains, not events, so a busy writer cannot trip it with a single mistimed poll. Past the recovery threshold the tracker bounces the subscriber and the streak resets — so a sawtooth is the recovery working, and a flat non-zero plateau is it failing.
- `pg_fanout_eventlog_poll_saved_total` — the running count of items the fallback poll relayed before a notification arrived. Read it as a rate: a permanently climbing line is a deployment where the notification path has never worked, which is what pointing `CONN_STRING` at PgBouncer produces.

Both are per replica and neither is leader-only. A single replica showing a streak while the others are at zero is that replica's connection; every replica showing one at once is the database or the connection path.

## Job lock

The only lock is `eventlog-prune`.

- `pg_job_lock_held{name="eventlog-prune"}` — 1 on the replica holding it, 0 on every other. **The sum across replicas should be exactly 1.** A sustained 0 everywhere means nobody is pruning and the eventlog is growing without bound; a sustained 2 would mean the lock is not doing its job, which has not been seen.
- `pg_job_lock_transitions_total{name="eventlog-prune"}` — acquisitions and releases. A steady low rate is replicas rolling; a high rate is the lock being stolen repeatedly, which means the holder is not renewing it in time.
- `pg_job_lock_restarts_total{name="eventlog-prune"}` — restarts after the pruner returned an error. **Any sustained rate here means pruning is failing persistently and only backoff is keeping it alive**; the eventlog is growing meanwhile.

## Connection pools

Every series carries a `pool` label: `main` is the pool queries run on, and `pubsub` is the direct pool, present only when `BOUNCER_CONN_STRING` puts queries on a separate pool. Without a bouncer there is one pool and it is `main`.

- `pgxpool_acquired_conns` against `pgxpool_max_conns` — **a `main` pool sitting at its maximum is the service queueing for connections**, and the next two series say how badly. `pgxpool_max_conns{pool="main"}` is `DB_MAX_CONNS`, 8 by default; `pubsub` is 2.
- `pgxpool_empty_acquires_total` and `pgxpool_empty_acquire_wait_seconds_total` — how often a caller found no idle connection, and how long it waited for one. Read them as rates. Zero is the healthy value; a sustained wait rate on `main` is an undersized `DB_MAX_CONNS`, and it presents as slow management RPCs and a lagging entry updater rather than as an error. Spellchecks are unaffected, since they never touch the pool.
- `pgxpool_total_conns` — **the `LISTEN` connection is not counted.** The subscriber hijacks it out of the pool, so `pubsub` reads 0 or 1 (the ping) with a healthy listener, and without a bouncer `main` reads one fewer than the connections Postgres sees.
- `pgxpool_canceled_acquires_total` — acquires abandoned because the caller's context ended first. A rate here alongside the wait is requests timing out on the pool.

## Task supervision

- `task_restarts_total` — restarts of the tasks under the `ErrGroup`. All four tasks in this service are `Required`, so this is close to a process-death counter: an increment means something exited and took the process with it. Read it against pod restarts; if they disagree, the difference is crashes that were not task exits.

## Health

- `health_check_up{name="custom_dictionary"}` — 1 once the replica has preloaded the custom dictionary and rules and caught up with the eventlog, 0 before that. It is a required check, so `/health/ready` fails while it is 0 and the replica gets no traffic; it only reads a flag, so it cannot fail because the replica is busy. A replica that stays at 0 is stuck in its preload; see [A replica does not become ready](ops.md#a-replica-does-not-become-ready). It never goes back to 0 once set, so **do not read it as dictionary freshness**: a dictionary that has stopped updating after startup produces no health-check signal at all. See [What is missing](#what-is-missing).
- `health_check_up{name="api_liveness"}` — registered by elephantine: the API listener answers `/health/alive`.

## Runtime

The Go and process collectors are the usual set, with one reading that is specific to this service:

- `go_memstats_heap_inuse_bytes` — the resident custom dictionary. Every replica holds the full custom dictionary and rule set in tries, per language. Hunspell's own dictionaries are allocated by libhunspell outside the Go heap, so they show in `process_resident_memory_bytes` and not here, and they grow with `spell_hunspell_handles`. **Heap here is a function of dictionary size and language count, not of request rate**, so a step up with flat traffic is an import that landed, and a slow climb across a week is the dictionary growing. It is the closest thing available to a gauge of loaded entries, which is a poor substitute for having one.

## What is missing

This is the honest list, and it is long enough to matter when reading everything above.

**Little measures spellchecking.** The hunspell handle pools say whether checks are queueing for hunspell, but there is no counter of checks by language, no histogram of matches per request, no gauge of entries or rules loaded. `rpc_duration_seconds` on `Check/Text` is the only evidence that the core of the service works at all.

**Nothing measures dictionary freshness.** The eventlog cursor a replica has reached is not exported, and neither is the newest event id in the database, so **eventlog lag — the one number that answers "is this replica serving the current dictionary?" — cannot be computed from metrics.** `pg_fanout_eventlog_poll_saved_streak` says the notification path is broken; it does not say how far behind a replica is, and it reads zero for a replica whose `entry_updater` is failing to drain for some other reason, because a failed drain relays nothing to count.

Until these exist, the logs are the instrumentation: `drain eventlog on notification`, `poll eventlog` and `prune eventlog` at error level, and `pruned eventlog` with a count at info.
