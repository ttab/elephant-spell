# Architecture

How elephant-spell is built: the process model, how a custom dictionary change reaches the tries that answer a spellcheck, each subsystem, and the RPC surface. Start here to understand the system before changing it.

| Document | What it settles |
|---|---|
| [`../README.md`](../README.md) | What the repository holds, how to build and run it, and what every configuration flag does. |
| **`architecture.md`** (this document) | How the service is built, and why. |
| [`ops.md`](ops.md) | Dependencies, deployment shape, bootstrap order, and the failure modes with the signal for each. |
| [`observability.md`](observability.md) | Every metric the service exports and what a change in it means. |
| [`../CONTEXT.md`](../CONTEXT.md) | What the words mean. |
| [`adr/`](adr/) | Why a decision went the way it did, and what was reversed. |

This document does not cover operating the service — no runbooks, no failure signals, no deployment shape. That is [`ops.md`](ops.md).

## Process model

`cmd/spell run` builds one `internal.Application` and hands it to `Application.Run`, which starts four tasks under an `elephantine.ErrGroup`. **All four are registered with `grp.Required`, so the exit of any one of them brings the process down.** There is no degraded mode in which the service serves spellchecks but has stopped following the dictionary.

| Task | What it does | Shutdown signal |
|---|---|---|
| `server` | HTTP: the three Twirp services, the web UI, and the profiling listener. | `CancelOnQuit` — drains in-flight requests. |
| `subscriber` | Owns the single `LISTEN` connection and its ping-based health check. | `CancelOnStop` |
| `entry_updater` | Preloads the dictionary, then follows the eventlog. | `CancelOnStop` |
| `eventlog_pruner` | Deletes events past the retention window, under a job lock. | `CancelOnStop` |

The two shutdown signals are not interchangeable. `server` stops on *quit*, which is the later of the two phases: it keeps serving while the background workers are already winding down, so a request in flight still finds a working spellchecker.

### Startup: dictionaries exist only during construction

`NewApplication` creates a temp directory, writes every embedded hunspell dictionary into it, and constructs one `hunspell.Checker` per language from the files. **The temp directory is then removed by a deferred `RemoveAll` before `NewApplication` returns.** This is not a leak being cleaned up late — hunspell reads the affix and dictionary files into its own memory at `Hunspell_create`, so by the time the constructor returns the files have no further purpose.

The consequence worth knowing: the service needs writable temp space at startup and none afterwards, and a dictionary cannot be swapped without a restart.

Language codes are derived from the filenames — `sv_SE.dic` becomes `sv-se` — so adding a language is a matter of dropping the pair into `dictionaries/` and nothing else.

### Two connection pools

`CONN_STRING` opens the direct pool. If `BOUNCER_CONN_STRING` is set and differs, a second pool is opened for general queries and the direct pool is left to the subscriber alone; otherwise both are the same pool.

**`LISTEN` cannot go through PgBouncer in transaction pooling mode** — the connection that registered the listener is returned to the pool and the notifications go nowhere — which is the whole reason the split exists. A deployment that points `CONN_STRING` at a bouncer has a service that starts cleanly, serves reads, and silently never sees a dictionary change until the fallback poll picks it up a minute later.

Neither pool sets `MaxConns`, so both take pgx's default of `max(4, runtime.NumCPU())`, read from the cpuset rather than the cgroup CPU quota. See [Pending work](#pending-work).

## Data flow

### The write path

Every mutating RPC — `SetEntry`, `DeleteEntry`, `RenameEntry`, `SetEntryStatus`, and the `Rules` equivalents — follows the same shape inside one transaction:

```
  RPC handler
      │  requireWriteScope(ctx)              spell_write, or permission_denied
      ▼
  BEGIN
      │  write the entry or rule row         entry / rule table
      │
      │  recordChange():
      │    LOCK TABLE eventlog IN EXCLUSIVE MODE
      │    INSERT INTO eventlog(...) RETURNING id
      │    pg_notify('eventlog', id)
      ▼
  COMMIT                                     row, event and notification
                                             become visible together
```

**The row, the event that announces it, and the notification are written in one transaction, and the exclusive table lock is what makes the log readable.** A writer holds the lock until commit, so the next writer cannot draw its event id until this event is committed and visible. That keeps commit order equal to id order — without it a transaction that drew id 7 could commit after one that drew id 8, and a consumer that had advanced its cursor to 8 would never see 7.

The lock serialises dictionary writes across the whole fleet. That is affordable here because dictionary edits are a human at a keyboard in the admin UI, not a machine-rate write path; it would not be if entries were ever bulk-loaded through the RPC rather than through the importer.

The published id is **only a wake-up**. The consumer ignores the payload and reads everything past its own cursor, so a lost or coalesced notification costs latency and never correctness.

### The read path: `entry_updater`

One goroutine per replica keeps that replica's in-memory tries in line with the database.

```
  ┌─ startup ────────────────────────────────────────────────┐
  │  1. cursor := GetLastEventID()                           │
  │  2. preloadEntries()   pages of 200, all languages       │
  │  3. preloadRules()                                       │
  │  4. drainEventlog(cursor)   catch up on startup writes   │
  └──────────────────────────────────────────────────────────┘
                              │
                              ▼
  ┌─ steady state ───────────────────────────────────────────┐
  │  select:                                                 │
  │    <-events        NOTIFY arrived  → drain               │
  │    <-ticker.C      every 1 minute  → drain, then         │
  │                                      updates.Polled(n)   │
  └──────────────────────────────────────────────────────────┘
```

**The cursor is read *before* the preload, and that order is the correctness argument.** The preload then sees a database state at least as new as the cursor, so every event up to the cursor is already reflected in what was loaded and only later events need replaying. Reading the cursor after the preload would open a window in which a write lands between the two and is both missed by the preload and skipped by the cursor. Events that land between the two reads are simply replayed, which is harmless — see below.

`drainEventlog` reads batches of 500 and keeps reading until a short batch tells it the log is exhausted, so a replica that has been unreachable catches up in one drain rather than one event per tick. The whole catch-up counts as a single `Polled` call regardless of backlog.

#### Replay is idempotent by construction

`applyEvent` never trusts the event's contents beyond its key. For a delete it removes the item; for an upsert it **re-reads the current row and applies that**, and if the row has since been deleted it removes the item instead. So applying events out of order, or applying the same event twice, converges on the current database state rather than on the state at the time the event was written.

This is what makes the preload/replay overlap safe, and it is why the eventlog can carry only `(language, entry, deleted, kind)` and no payload.

An event for a language this replica has no dictionary for is dropped silently.

### Recovery: when `LISTEN` is quietly dead

A TCP connection that is dead but not closed will accept a `LISTEN` and deliver nothing. Two mechanisms cover it, and they are independent:

- **The `Subscriber`'s own ping.** It notifies itself on a ping channel every `PING_INTERVAL` (default 5m) and reconnects if none arrives within `PING_GRACE` (default 7m).
- **The `FanOut` recovery tracker.** Every fallback-poll drain that *found work* without an intervening notification increments a streak; a notification resets it. Past the threshold the tracker calls `subscriber.Bounce` to rebuild the connection.

The second is the interesting one, because it catches the case the ping cannot: a connection healthy enough to carry the service's own pings but not the notifications it is subscribed to. The signal is `pg_fanout_eventlog_poll_saved_streak` — see [observability.md](observability.md#eventlog-fanout).

### `eventlog_pruner`

Runs under the `eventlog-prune` job lock so exactly one replica prunes, ticks every 15 minutes, and deletes events older than **one hour**.

An hour is short for an eventlog, and deliberately so. **Nothing recovers from the eventlog across a restart** — a replica that comes back reloads full state from the `entry` and `rule` tables and resumes from the latest id. The window therefore only has to exceed consumer lag on a *running* replica: the one-minute fallback poll plus listener reconnect and bounce recovery. It is not a retention policy in the elephant-repository sense and nothing downstream may treat it as one.

A failed prune is logged and not fatal; the lock is kept and the next tick retries.

## The spellchecker

One `*Spellcheck` per language, each holding a hunspell instance, four tries, a rule map and a buffer pool, behind a single `sync.RWMutex`. Checks take the read lock; eventlog application takes the write lock.

### Four tries, not one

| Trie | Holds |
|---|---|
| `trie` | Case-sensitive phrases, keyed by exact text. |
| `mistakeTrie` | Case-sensitive common mistakes. |
| `ciTrie` | Case-insensitive phrases, keyed by case-folded text. |
| `ciMistakeTrie` | Case-insensitive common mistakes. |

Entries are case-insensitive by default: a lowercase common mistake is caught when the word appears capitalised at the start of a sentence, and the suggestion takes on the matched word's leading capital. An entry that sets `case_sensitive` is routed to the exact-case pair instead, which is what a proper noun needs.

**An entry lives in exactly one pair, chosen at insert time**, so a lookup consults both and the routing decision is never made on the hot path.

### Phrase matching

A sliding window of up to three words runs over the text via `PhraseIterator`, and each window is looked up in the tries. Longer matches win: `resolveLongest` sorts candidates by length and suppresses any that overlap an accepted longer one, so "Mexico City" is not also reported as a match on "Mexico".

Context guards are evaluated after the trie has located a match — `before`, `after`, `not_before`, `not_after` against the neighbouring word — which is cheap precisely because it happens last.

### Pattern rules

Rules match token patterns the tries cannot express: number ranges, gaps between words, context-dependent corrections. A pattern compiles to a safe RE2 regex; `{digit}`, `{word}` and `{gap(N)}` are placeholders and **everything else, whitespace included, is matched literally and case-insensitively**. So `{digit}-{digit}` matches `12-15` and not `12 - 15`.

#### This replaced a token-based matcher

The first implementation segmented the text into tokens and matched against those, which meant whitespace was ignored inconsistently — `5kr` never split into `5` and `kr`. Making whitespace significant was the fix, and it is why the pattern syntax is brace-delimited rather than token-delimited. Do not reintroduce a matcher that runs the word segmenter before pattern matching; see [ADR-0001](adr/0001-patterns-compile-to-regex.md).

Rules are keyed by a generated sequential id, not by name. The name is a non-unique human-readable label that can be edited and duplicated; see [ADR-0002](adr/0002-rules-keyed-by-id.md) for what keying on the name cost.

## The web UI

Built on [howdah](https://github.com/ttab/howdah): OIDC login, a `PageMux` for the UI routes, and the service's own RPC implementations called in-process rather than over the wire. `bridgeServiceAuth` turns the browser session into the `AuthInfo` the RPC handlers expect.

The sections are Dictionaries, Rules, Moderation, Spellcheck (a scratch pad for trying text against the current state) and Documentation.

**Two protections come from the `PageMux` and are not configurable**: a cross-site `POST`/`PUT`/`PATCH`/`DELETE` is refused with a 403, and every UI response carries `Content-Security-Policy: frame-ancestors 'none'`. Anything that must be posted to from another origin belongs on the bare `http.ServeMux` instead.

### Sessions are sealed, and that needs a key

howdah has no mode that writes an OAuth2 refresh token to the browser in the clear. The session cookie is sealed with AES-256-GCM under a keyring read from `COOKIE_KEY_*` at startup, and **the service refuses to start without at least one currently usable key**. See the README's [Cookie keys](../README.md#cookie-keys) for the format and the rollover.

### The guide is served out of `docs/guide/`

`docs/docs.go` embeds `guide/*.md` and `DocsUI` renders **every** markdown file it is handed into a page in the admin UI navigation. The embed is scoped to `guide/` for exactly that reason: `architecture.md` sitting next to `dictionaries.md` would be published to the quality desk. `docs/adr/` and `docs/agents/` are safe because the embed is not recursive, but a new top-level `docs/*.md` is only safe because it is not in the embed list.

## RPC surface and scopes

Three Twirp services, all mounted through `elephantine.APIServer` with `ServiceAuthRequired`, so **every RPC needs a valid token** and an anonymous caller is refused before the handler runs.

| Service | RPCs | Scope |
|---|---|---|
| `elephant.spell.Check` | `Text`, `Suggestions` | Any authenticated caller. |
| `elephant.spell.Dictionaries` | `SupportedLanguages` | Any authenticated caller. |
| | `GetEntry`, `ListEntries`, `ListDictionaries`, `SetEntry`, `SetEntryStatus`, `RenameEntry`, `DeleteEntry` | `spell_write` |
| `elephant.spell.Rules` | `GetRule`, `ListRules`, `SetRule`, `SetRuleStatus`, `DeleteRule` | `spell_write` |

`requireWriteScope` is the single gate for the second and third groups.

### One implementation, two mounts

Each service is mounted twice: `RegisterAPI` puts it on `/twirp/elephant.spell.<Service>/` and `RegisterConnect` on `/elephant.spell.<Service>/`. Both are served by the same `*Application` — the generated `spellconnect.New<Service>ServiceHandler` adapts the plain `spell.<Service>` interface, so there is no second set of methods to keep in step.

**The handlers are written in the Connect error vocabulary and never name a protocol.** `rpc.RequiredArgument`, `rpc.NotFound`, `rpc.Internalf` and the rest produce `*connect.Error` values; the Connect mount renders them directly, and the Twirp mount's `rpc.TwirpInterceptor` — installed for you by `ServiceOptions.ServerOptions` — translates code, message and meta on the way out. A handler that reaches for a `twirp.*` error instead would answer a Connect caller with an uncoded `unknown`.

**The web UI is the exception, and it is why `rpcErrorToHTTP` reads Connect codes.** The UI calls the RPC implementations in process, so no mount and therefore no interceptor stands between them: it sees the `*connect.Error` itself.

`rpc_protocol_responses_total{protocol=...}` is what says when the Twirp mount can be retired; see [observability.md](observability.md#rpc-surface).

An unauthenticated or unparseable token is answered `unauthenticated` (401). `permission_denied` (403) is reserved for a caller we did identify and that lacks `spell_write`.

`custom_only` on `Text` and `Suggestions` skips the hunspell pass entirely, so a caller can surface the editorial rules without hunspell's own corrections mixed in.

## Schema

| Table | Holds |
|---|---|
| `entry` | Custom dictionary entries, keyed by `(language, entry)`. Forms, common mistakes, guards and the rule body live in a jsonb `data` column. |
| `rule` | Pattern rules, keyed by a sequential id, with `language` indexed. |
| `eventlog` | The change feed. `(language, entry, deleted, kind)`; no payload. `kind` routes an event to the word or the rule store — rules used to live on the entry row, and [ADR-0003](adr/0003-rules-live-in-their-own-table.md) says what that cost. |
| `job_lock` | The `eventlog-prune` lock, and any future one. |
| `schema_version` | tern's migration state. |

Queries are compiled by sqlc from `postgres/queries.sql`. `postgres/entry.go` is the hand-written exception, holding the `data` column's Go types.

**Migrations are never applied by the service.** `mage sql:migrate` locally, `go run ./cmd/setup db migrate` in elephant-platform.

## Pending work

**No metrics of its own.** The service registers no collectors: everything on `/metrics` comes from elephantine, the job lock, the FanOut recovery tracker and the Go runtime. There is no counter of spellchecks by language, no gauge of entries or rules loaded per replica, and no measure of eventlog lag — so "is this replica's dictionary current?" is not answerable from monitoring, only from the logs. See [observability.md](observability.md#what-is-missing).

**No pool statistics.** Neither pool is registered with `pg.NewPoolStatCollector`, so pool saturation — the thing most likely to make the service slow rather than broken — is invisible.

**No readiness check.** Nothing is registered with `AddReadyFunction` or `AddOptionalReadyFunction`; `/health/alive` is elephantine's and answers as soon as the HTTP server is up. A replica that is still preloading, or whose `entry_updater` is failing to drain, reports itself alive and takes traffic with a stale or empty dictionary.

**Unbounded pool sizing.** Neither pool sets `MaxConns`, so each takes `max(4, runtime.NumCPU())` read from the cpuset. On Kubernetes with the default CPU manager policy that tracks the *node's* vCPU count and changes on reschedule, which makes the pool size a property of where the pod landed.
