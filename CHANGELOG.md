# Changelog

Everything from v1.0.0 forward is documented here; the releases before it are
in the git history only. Entries are derived from the release tags, and the
linked PRs hold the detail.

## [v1.7.0] - Unreleased

**Behaviour change (the connection pools are sized explicitly):** the pool
queries run on is now sized by `DB_MAX_CONNS` (`--db-max-conns`), default 8,
where both pools used to take pgx's `max(4, NumCPU())` read from the node's
cpuset, so their size changed with the node a pod landed on. That pool is the
`BOUNCER_CONN_STRING` pool when one is set and the `CONN_STRING` pool
otherwise. With a bouncer the direct pool, which carries only the `LISTEN`
session, is fixed at 2. A `pool_max_conns` in a connection string is
overridden; setting `DB_MAX_CONNS=0` restores pgx's own sizing.

Changes:

- Both connection pools are exported as `pgxpool_*` series, labelled
  `pool="main"` and — when `BOUNCER_CONN_STRING` makes it a separate pool —
  `pool="pubsub"`. See
  [connection pools](docs/observability.md#connection-pools).

## [v1.6.0] - 2026-09-16

**New (the services are served on Connect as well as Twirp):** each of `Check`, `Dictionaries` and `Rules` is now mounted twice — on `/twirp/elephant.spell.<Service>/` as before, and on `/elephant.spell.<Service>/` for Connect and gRPC. Both are served by the same implementation, so behaviour, scopes and error codes are identical; nothing about the Twirp mount changes and no caller has to move. A Go client moves by swapping `spell.New<Service>ProtobufClient` for `spellconnect.New<Service>ServiceClient`, which satisfies the same interface. (ELE-1504)

**Behaviour change (Connect JSON spells field names differently):** a Connect JSON response uses lowerCamelCase (`customOnly`, `caseSensitive`) where a Twirp JSON response uses the spelling from the `.proto` (`custom_only`, `case_sensitive`). Requests are accepted either way and generated clients are unaffected. **A caller that reads JSON by hand with `fetch` or `curl` and changes only the path prefix gets a `200` and `undefined` for every multi-word field.** This is deliberate: the standard encoding is what every Connect runtime assumes, so the service does not install a `UseProtoNames` codec to make Connect look like Twirp.

**Breaking (the web UI needs a cookie keyring):** the session cookie is now
sealed with AES-256-GCM, and the service refuses to start without at least one
currently usable key. Provision `COOKIE_KEY_1` before the deploy, in the form
`<RFC 3339 use-after>_<base64 of 32 random bytes>` — `howdah.GenerateCookieKey`
produces a secret, and `COOKIE_KEY_2`, `COOKIE_KEY_3` and so on hold the keys
of a rollover in progress. Everyone with a session is logged out once on
upgrade, since the unencrypted cookies the old howdah wrote cannot be read.
Every cookie the UI sets now also carries `Secure` unconditionally, so a
deployment served over plain HTTP — local development, in practice — has to set
`INSECURE_COOKIES=true` or the browser will never send the session back. The
README's "Cookie keys" section carries the format and the rollover.

**Behaviour change (an unusable token is answered 401):** a request whose
`Authorization` header cannot be authenticated is now answered `unauthenticated`
(401) where it was `permission_denied` (403). `permission_denied` is left to
mean a caller we did identify and that lacks `spell_write`. Anything keyed on
403 for a bad token — an ingress rule, a dashboard panel, a client's retry
logic — reads 401 after the upgrade. The check also now runs as HTTP middleware
ahead of the handler, so an unauthenticated caller no longer gets a request body
unmarshalled on its behalf.

**Behaviour change (the web UI refuses cross-site writes and framing):** a
`POST`, `PUT`, `PATCH` or `DELETE` to a UI route is answered with a 403 error
page unless the browser says the request came from this application, and every
UI response carries `Content-Security-Policy: frame-ancestors 'none'`. Requests
carrying neither `Sec-Fetch-Site` nor `Origin` are not browser requests and are
let through, so the RPC services and health checks are unaffected. Anything
that embeds the spell UI in an iframe stops working.

**Build (the Go floor is 1.27.1):** the `go` directive moves from 1.26.4 to
1.27.1, and the Docker images move from Debian bookworm to trixie —
`golang:1.27.1-trixie` to build and `debian:trixie-slim` to run, which is
hunspell 1.7.2 in both. A build of this service needs a Go 1.27 toolchain and
`libhunspell-dev` from trixie.

Changes:

- The locale files gain `SessionUnavailable` and `CrossSiteRequestBlocked`,
  the two error messages howdah's new failure modes render. Both have English
  fallbacks, so a missing translation degrades rather than breaks.
- `--insecure-cookies` / `INSECURE_COOKIES` is new, and `--default-language`
  is now documented in the README's configuration table alongside it.
- Dependency upgrades: howdah to v0.5.0, elephantine to v0.29.1, elephant-api
  to v0.25.0, eltest to v0.5.0, mage to v0.13.1, pgx, prometheus, OpenTelemetry
  and the `golang.org/x` suite to their current releases.
- The eventlog prune job moves onto `elephantine/pg/joblock`, which is where
  the job lock now lives. The lock name, the interval and the retention window
  are unchanged, so there is nothing to reconfigure.
- The RPC handlers are written in the `elephantine/rpc` error vocabulary rather than constructing Twirp errors directly, which is what lets one implementation answer both stacks with the right code. The web UI reads those errors as the `*connect.Error` values they are, since it calls the implementations in process.
- The integration suite is parameterised over the two stacks with `TEST_RPC_STACK`, and CI runs it against both, so every existing test is also a Connect test.
- Repository: a `.golangci.yml` matching the rest of the fleet, where the
  linter previously ran on its defaults. Its findings are fixed rather than
  suppressed, bar two gosec false positives, the diagnostics cgo raises in its
  own generated wrappers and attributes back to the calling line, and misspell,
  which must not run on the tests of a spellchecker — its `--fix` rewrites the
  deliberate misspellings they are built from. Test
  files that reach into unexported identifiers are renamed to
  `*_internal_test.go`, which is what keeps them legitimately in the `internal`
  package.
- Repository: this changelog.

## [v1.5.0] - 2026-06-09

**Behaviour change (moderation status reaches Check responses):** a custom
entry's status is now surfaced on `Check` responses, so a client can tell a
correction that comes from a reviewed entry from one that is still pending.
Entries are used for spellchecking regardless of status, as before; what is new
is that a client can see which. (#95)

**Behaviour change (custom entries match case-insensitively):** a lowercase
common mistake is now caught when the word appears capitalised, and the
suggestion takes the matched word's leading capital. A proper noun opts back
into exact-case matching with the new per-entry case-sensitive flag. (#95)

**Behaviour change (dictionary search is free-text):** the dictionary filter
searches the entry text, its description and its common mistakes as a
substring, where it previously matched an entry-text prefix only. The request
field is renamed `prefix` to `query`. (#95)

**Migrations:** run all three before the deploy; none of them takes a lock
worth planning around or needs a maintenance window. (#94, #95)

- `005_eventlog.sql` — creates the eventlog table the fanout sync reads.
- `006_rules.sql` — creates the rule table and migrates the rules that were
  stored on entries into it.
- `007_entry_case_sensitive.sql` — adds the per-entry case-sensitivity flag.

Changes:

- A token rule engine matches patterns over the text rather than exact
  strings, for the errors a trie cannot express: number ranges, gaps between
  words, and context-dependent corrections. Patterns are `{digit}`, `{word}`
  and `{gap(N)}` placeholders with everything else — whitespace included —
  matched literally, compiled to a safe RE2 regex; captures are referenced in
  the replacement as `{1}`, `{2}` and so on. Context guards suppress or
  require a match based on the neighbouring word. The number-range en dash
  rule ships built in. Rules live in their own table keyed by a sequential id,
  are managed by a new `elephant.spell.Rules` service, and sync through the
  eventlog like words do. (#95)
- The web UI gains a per-language moderation queue listing pending words and
  rules together, a `/rules` section, and an entry form with structured
  incorrect→correct rows and a highlighting editor for the `{A|B}` expansion
  syntax with a live count and a full expansion list. (#95)
- Custom dictionary state now syncs through an eventlog with a fanout rather
  than a full reload per notification, so a replica applies the changes it
  missed instead of rebuilding. (#94)
- The web UI is usable on a phone.
- The documentation splits into `dictionaries.md` and `rules.md` behind an
  index, and every markdown file under `docs/` is now served. (#95)

## [v1.2.2] - 2026-05-22

Changes:

- `custom_only` on `Text` and `Suggestions` skips the hunspell pass entirely,
  so a caller can surface writing rules without hunspell's corrections mixed
  in. (#89)
- Dependency upgrades.

## [v1.2.1] - 2026-04-30

**Breaking (TLS configuration):** the certificate and key are read from
`TLS_CERT_PATH` and `TLS_KEY_PATH`, aligning the names with the rest of the
fleet. A deployment setting the old names serves without TLS after the
upgrade. (#85)

## [v1.2.0] - 2026-04-21

**New (a web UI for the custom dictionaries):** the service now serves a
howdah-based UI for browsing and editing custom entries, behind OIDC login with
`spell_write` for writes, in English, Swedish and Norwegian. It needs
`OIDC_PROVIDER`, `CLIENT_ID`, `CLIENT_SECRET` and `CALLBACK_URL`, all of them
required, and `--default-language` picks the language the root page redirects
to. (#80)

**Migrations:** (#80)

- `004_entry_updated.sql` — adds the `updated` and `updated_by` columns that
  record who last changed an entry. Run it before the deploy; it adds nullable
  columns and needs no maintenance window.

Changes:

- The entry status is a select of `pending` and `accepted` rather than free
  text, and the entry list filters and paginates server-side. (#80)
- Go moves to 1.26.2, and the Docker build actions to their current major
  versions. (#81)
- A CSV importer for bulk-loading entries. (#66)
- Worked around a bug in the trie library's delete, which left stale entries
  behind. (#67)

## [v1.1.0] - 2026-02-16

**Breaking (the spellcheck model):** `Check` returns a richer result and
suggestions are a request of their own rather than part of a spellcheck
response. Entries gain correction levels, alternate forms, and `{A|B}`
combinatoric expansion of common mistakes. A client reading the old response
shape has to move. (#51)

**New (TLS):** the service can serve TLS itself, on `TLS_ADDR` with a
certificate and key from the environment. (#55)

**Behaviour change (two database connections):** `LISTEN/NOTIFY` needs a direct
connection, so general queries can now go through PgBouncer on
`BOUNCER_CONN_STRING` while `CONN_STRING` stays direct. A deployment that sets
only `CONN_STRING` is unchanged. The listener also sends itself periodic pings
and reconnects with a full reload if one fails to arrive within the grace
period, which is what catches a connection that TCP keepalive does not.
`PING_INTERVAL` and `PING_GRACE` tune it. An entry update that cannot be
applied is now fatal, so the process restarts rather than serving quietly stale
state. (#65)

**Migrations:** run both before the deploy; neither needs a maintenance
window. (#51, #65)

- `002_forms.sql` — adds the alternate-forms storage the richer entry model
  uses.
- `003_job_lock.sql` — creates the job lock table the ping sender takes its
  advisory lock in.

## [v1.0.1] - 2025-06-18

Changes:

- Dependency upgrades.

## [v1.0.0] - 2025-05-10

The first release. A spellcheck service combining hunspell with a custom
dictionary in PostgreSQL, exposing `elephant.spell.Check` for checking text and
`elephant.spell.Dictionaries` for managing custom entries, with bundled
dictionaries for Swedish, British and American English, Danish, Finnish,
Norwegian Bokmål and Nynorsk.
