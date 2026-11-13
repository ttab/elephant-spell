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

### Startup: one hunspell handle per language

`NewApplication` constructs one `hunspell.Checker` per language with `hunspell.NewCheckerFS`, straight from the embedded dictionaries. Hunspell only reads from the file system, so loading a handle writes the language's `.aff` and `.dic` to a fresh temp directory, calls `Hunspell_create`, which reads both files into its own memory, and removes the directory again. A missing file fails construction: `Hunspell_create` itself reports nothing and would return a handle that accepts no words, so the checker checks the files first.

Each checker loads one handle at startup and more later, under load (see [the hunspell handle pool](#the-hunspell-handle-pool)), so the service needs writable temp space whenever a handle is loaded, not only at startup. A dictionary cannot be swapped without a restart.

Language codes are derived from the filenames — `sv_SE.dic` becomes `sv-se` — so adding a language is a matter of dropping the pair into `dictionaries/` and nothing else.

### Two connection pools

`CONN_STRING` opens the direct pool. If `BOUNCER_CONN_STRING` is set and differs, a second pool is opened for general queries and the direct pool is left to the subscriber alone; otherwise both are the same pool.

| | Queries run on | Query pool size | Direct pool size | Registered as |
|---|---|---|---|---|
| No bouncer | the direct pool | `DB_MAX_CONNS` | (the same pool) | `main` |
| Bouncer | the bouncer pool | `DB_MAX_CONNS` | 2 | `main`, `pubsub` |

**`LISTEN` cannot go through PgBouncer in transaction pooling mode** — the connection that registered the listener is returned to the pool and the notifications go nowhere — which is the whole reason the split exists. A deployment that points `CONN_STRING` at a bouncer has a service that starts cleanly, serves reads, and silently never sees a dictionary change until the fallback poll picks it up a minute later.

**Both pools are sized explicitly**, because pgx's own default of `max(4, runtime.NumCPU())` reads the cpuset rather than the cgroup CPU quota — on Kubernetes that is the node's vCPU count, which makes the pool size a property of where the pod landed. `DB_MAX_CONNS` sizes the pool queries run on and defaults to `DefaultDBMaxConns`, 8. Spellchecking never touches the database, so that pool carries only the dictionary and rule management RPCs — whose writes serialise on the eventlog lock anyway — the entry updater, the pruner and its job lock, and the subscriber's ping when there is no bouncer. The comment on the constant has the arithmetic. With a bouncer the direct pool is `ListenPoolMaxConns`, 2: the `LISTEN` session, which the subscriber hijacks out of the pool, and the ping.

Both are registered with `pg.NewPoolStatCollector`, the query pool as `pool="main"` and the direct pool as `pool="pubsub"` only when it is a separate pool; see [observability.md](observability.md#connection-pools).

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
  │  2. preloadEntries()   pages of 200 by key, all languages│
  │  3. preloadRules()     pages of 200 by id                │
  │  4. drainEventlog(cursor)   catch up on startup writes   │
  │  5. loaded = true      /health/ready passes              │
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

The preload pages by key — entries after the last `(language, entry)` read, rules after the last id — rather than by offset. With an offset, a row deleted from a page already read would shift the next unread row back onto that page, and no event would bring it back, since nothing about that row changed.

**The replica is not ready until step 5.** `Run` registers a required readiness check, `custom_dictionary`, on the API server's health server, and it fails until the preload and the initial drain have completed, so a replica that is still loading is kept out of service rather than answering checks against a partial dictionary. The check only reads a flag the updater sets, so it competes with request handling for nothing and cannot fail because the replica is busy — the reason it can be required.

`drainEventlog` reads batches of 500 and keeps reading until a short batch tells it the log is exhausted, so a replica that has been unreachable catches up in one drain rather than one event per tick. The whole catch-up counts as a single `Polled` call regardless of backlog.

#### Replay is idempotent by construction

`applyEvent` never trusts the event's contents beyond its key. For a delete it removes the item; for an upsert it **re-reads the current row and applies that**, and if the row has since been deleted it removes the item instead. So applying events out of order, or applying the same event twice, converges on the current database state rather than on the state at the time the event was written.

This is what makes the preload/replay overlap safe, and it is why the eventlog can carry only `(language, entry, deleted, kind)` and no payload.

An event for a language this replica has no dictionary for is dropped silently. A rule event goes to the language in the rule's row, not the one recorded on the event, and the rule is taken out of every other language's spellchecker; `SetRule` refuses an update whose language differs from the stored one, so the two only disagree in an event written by an older version.

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

One `*Spellcheck` per language, each holding a hunspell checker with its pool of handles, four tries, the loaded entries by text, a rule map and a count of the words the entries have added to hunspell, behind a single `sync.RWMutex`. Checks take the read lock; eventlog application takes the write lock.

### Four tries, not one

| Trie | Holds |
|---|---|
| `trie` | Case-sensitive phrases, keyed by exact text. |
| `mistakeTrie` | Case-sensitive common mistakes. |
| `ciTrie` | Case-insensitive phrases, keyed by case-folded text. |
| `ciMistakeTrie` | Case-insensitive common mistakes. |

Entries are case-insensitive by default: a lowercase common mistake is caught when the word appears capitalised at the start of a sentence, and the suggestion takes on the matched word's leading capital. An entry that sets `case_sensitive` is routed to the exact-case pair instead, which is what a proper noun needs.

**An entry lives in exactly one pair, chosen at insert time**, so a lookup consults both and the routing decision is never made on the hot path.

**A key holds every entry that claims it.** Two entries can share a common mistake, one entry's text can be another's form target, and two case-insensitive entries can differ only in case ("Polen" and "polen" are two rows, since the primary key is case-sensitive). A match reports the suggestions of all the entries on its key, deduplicated, with the level and status of the first entry at the most severe level. An entry is identified by its exact text, so updating or removing it takes out its own claims and leaves the other entries on a shared key alone.

### The hunspell handle pool

A hunspell handle is not safe for concurrent use, and a suggestion search on one takes 20–110 ms per misspelled word in Swedish, so a single handle per language would serialise every check that runs hunspell. **Each `hunspell.Checker` holds a pool of handles loaded from the same dictionary** and lends one to each `Spell`, `Suggest` or `Stem` call, which keeps it until the call is done, including freeing hunspell's result list.

The pool starts with one handle and grows to at most `HUNSPELL_HANDLES` (`hunspell.DefaultHandles`, 4). When a call finds every handle busy it starts loading one more in the background, if the pool has room, and waits for whichever handle comes free first; the wait ends with the caller's context. Loading continues while calls are still waiting. A pool never shrinks. **Growing on contention keeps the memory where the traffic is**: an extra handle costs as much as the first one (measured RSS: fi-fi 41 MiB, nb-no 20 MiB, nn-no 13 MiB, da-dk 12 MiB, sv-se 9 MiB, en-gb and en-us 4 MiB each), so a full pool for all seven languages would add about 310 MiB and three seconds of startup, where in practice only the languages under concurrent load grow.

**Custom words reach every handle at once.** `Add` and `Remove` take the checker's write lock, take every handle out of the pool, change each one, and put them all back, so a call sees either none of the handles changed or all of them, and never a handle that is half-way through an update. While that happens, checks for the language wait. Every update is also kept in an ordered log that a newly loaded handle replays before it is handed out. The log is not compacted, because hunspell's add and remove change a word's affix flags, so their combined effect depends on the order of the whole history.

**Words are only removed from hunspell if an entry put them there.** An entry's text and form targets are added to hunspell so the hunspell pass accepts them. Each such word is reference-counted across entries, and on the first claim the spellchecker records whether hunspell already accepted it. A word is removed only when the last claim goes away and hunspell did not know it before, because hunspell's remove marks a word forbidden: removing a base dictionary word would make it and its inflections misspelled until restart.

### Phone numbers, email addresses and URLs are masked first

`Check` blanks out these before any pass runs. The segmenter would otherwise take them apart at `/`, `@` and `-` and hand hunspell the pieces (`forms`, `gle`, `ouiwJ6gJapE98Rru5`), and the `{digit}-{digit}` dash rule would fire inside a phone number. `ignoredSpans` finds them as whitespace-delimited tokens in the raw text, with wrapping punctuation trimmed, and `maskSpans` overwrites each one with spaces, one per byte, so the byte offsets every pass reports still address the source text and the reported spans are computed from the source.

The shapes are deliberately narrow, because a false positive here hides a real error:

| Ignored | Not ignored |
|---|---|
| `+46`, `0720-349665`, `08/692`, `010-123` — digits joined by `+ - / ( )`, starting with `+` or with a `0` trunk prefix of at least two digits and five digits in all | `0-2`, `08-12`, `1990-1995`, `2024-11-03`, a plain number |
| `name@example.com`, `mailto:info@tt.se` | `@ttnyheter` |
| `https://…`, `www.tt.se`, `forms.gle/abc`, `tt.se` | `t.ex.`, `p.g.a.`, `kl.14`, `Rom.Sedan` — a bare domain needs labels of two or more characters and a lowercase top-level domain |

A word that mixes letters and digits (`2o24`) is not a number and is still checked. This runs on the raw text rather than on tokens for the same reason the rules do; see [ADR-0001](adr/0001-patterns-compile-to-regex.md).

### Phrase matching

A sliding window of up to five words (`MaxPhraseWords`) runs over the text via `EachPhrase`, and each window is looked up in the tries. Letter tokens, number tokens and tokens that mix the two count as words, so an entry can start or end on a number ("Formel 1", "covid-19", "G-7"); only letter tokens go on to hunspell, so a number is never reported as misspelled.

`SetEntry` and `RenameEntry` refuse an entry text, an expanded common mistake, or either side of a form with more words than the window holds, with `InvalidArgument` naming the field, since such a phrase could never match. Words are counted by `PhraseWordCount`, the same way the window counts them. A stored row that is over the limit, written before the check existed, is skipped with a warning when it is loaded, by the preload and by the eventlog alike, rather than failing the load.

An entry's common mistakes may expand to at most `MAX_EXPANSIONS` strings in total (`DefaultMaxExpansions`, 200), all of its `{A|B}` patterns together, because every expansion is a trie key held by every replica: a hundred bytes of `{a|b}` groups would otherwise expand to 2^20 strings. `Expand` multiplies the option counts before generating anything and returns an `*ExpansionLimitError` when the product is over its limit, so a pathological pattern costs nothing to refuse. `SetEntry` refuses an entry over the limit with `InvalidArgument` on `entry.common_mistakes`, naming the count and the limit; the entry form's preview and expansion list count without expanding and mark the line that takes the entry over the limit as an error. A stored row over the limit (written before the check, or under a higher limit) is skipped with the same "skip entry that cannot be matched" warning at load and from the eventlog. `AddPhrase` also stops at the limit, and logs each common mistake it skips, whether for the limit or because it fails to parse. The leading-case variants a case-sensitive entry gets as implicit common mistakes do not count towards the limit, since the editor cannot see them, but there are at most 63 of each phrase (only phrases with up to six capitalised words get them), so an entry has at most 64 × (limit + 1) mistake keys.

The word segmenter buffers its whole input and returns nothing but an error for anything over 64 KiB, so `segmentTokens` feeds it the text in pieces of at most 32 KiB, cut after ASCII whitespace where the next character always starts a new token, and offsets every token back into the whole text. Both the trie pass and the hunspell pass read the text through it. The window carries over from one piece to the next, so a phrase that straddles a cut still matches. A run of more than 32 KiB with no place to cut is passed on as a single non-word token: it is not checked, and the text around it is. A segmenter error fails the check rather than producing an empty result. Longer matches win: `resolveLongest` sorts candidates by length and suppresses any that overlap an accepted longer one, so "Mexico City" is not also reported as a match on "Mexico". It tracks the area the longer matches cover rather than comparing every pair, and the spans are converted to rune offsets in one pass over the text, so a check stays close to linear in the number of matches.

Context guards are evaluated after the trie has located a match — `before`, `after`, `not_before`, `not_after` against the neighbouring word — which is cheap precisely because it happens last. The neighbouring word is a run of letters and ASCII digits separated from the match by ASCII whitespace only, found by walking outwards from the match, so a guard looks at the few characters next to the match and not at the rest of the text.

`Check` stops with the context's error once the caller's context ends: between words in the hunspell pass, every 1024 phrases in the trie pass and between rules, and `Text` checks it between the texts of a request. A request the client has given up on no longer keeps hunspell busy.

Every valid phrase and every reported mistake is then hidden from the hunspell pass by masking its byte span with `maskSpans`, so only that occurrence is skipped: a valid entry "hen" does not take the letters out of "hennes".

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

Every RPC lower-cases the language code it is given, so `SV-SE` and `sv-se` name the same dictionary for writes, reads and checks alike. A write (`SetEntry`, `SetEntryStatus`, `RenameEntry`, `DeleteEntry`, `SetRule`) refuses a language with no spellchecker with `InvalidArgument`. A rule keeps the language it was created in: `SetRule` refuses an update that names another one.

`ListEntries` and `ListRules` return pages of `page_size` rows, 100 when it is unset and at most `MaxPageSize` (500) — a larger size is capped, not refused. A negative `page` is refused with `InvalidArgument`.

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

**No metrics of its own.** The service registers no collectors beyond the pool statistics `main` wires up: everything else on `/metrics` comes from elephantine, the job lock, the FanOut recovery tracker and the Go runtime. There is no counter of spellchecks by language, no gauge of entries or rules loaded per replica, and no measure of eventlog lag — so "is this replica's dictionary current?" is not answerable from monitoring, only from the logs. See [observability.md](observability.md#what-is-missing).

**Readiness covers startup only.** The `custom_dictionary` check keeps a replica out of service until its first preload and drain are done, but once set it stays set: a replica whose `entry_updater` later fails to drain keeps taking traffic with a stale dictionary.
