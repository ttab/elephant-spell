# Elephant spell

Spellcheck service combining [hunspell](https://hunspell.github.io/) with an editor-managed layer stored in PostgreSQL.

The editor-managed layer has two kinds of item. **Entries** are words and phrases with their common mistakes, alternate forms and context guards; **rules** are patterns with placeholders, for the errors a word list cannot express — number ranges, spacing, context-dependent corrections. Both are moderated by the quality desk before they are marked reviewed, and both are used for spellchecking either way.

Three services expose it — `Check`, `Dictionaries` and `Rules` — each served on both a [Twirp](https://github.com/twitchtv/twirp) and a [Connect](https://connectrpc.com/) mount, alongside a web UI where the dictionaries, the rules, the moderation queue and a spellcheck scratch pad live. Every replica holds the whole editor-managed layer in memory and follows a Postgres eventlog to stay current.

## Documentation

| Document | What it settles |
|---|---|
| **`README.md`** (this document) | What the repository holds, how to build and run it, what every configuration flag does, and what is missing. |
| [`docs/architecture.md`](docs/architecture.md) | How the service is built: the process model, the write and read paths, the spellchecker, and the RPC surface. |
| [`docs/ops.md`](docs/ops.md) | Dependencies, deployment shape, bootstrap order, and the failure modes with the signal for each. |
| [`docs/observability.md`](docs/observability.md) | Every metric the service exports and what a change in it means. |
| [`CONTEXT.md`](CONTEXT.md) | What the words mean, and which of them mean something else in the platform. |
| [`docs/adr/`](docs/adr/) | Why a decision went the way it did, and what was reversed. |

`docs/guide/` is not part of that set: it is the quality desk's guide to writing entries and rules, embedded into the binary and served in the admin UI at `/docs/`.

Links between these are checked mechanically:

```bash
go run github.com/magefile/mage docs:links
```

## Repository layout

```
cmd/spell/          the service binary
cmd/spell-client/   the operator CLI
internal/           the application: RPC handlers, spellchecker, eventlog consumer, web UI
hunspell/           the cgo binding
dictionaries/       the embedded hunspell dictionaries, one .aff/.dic pair per language
postgres/           sqlc-generated queries; entry.go is hand-written
schema/             tern migrations
templates/          the web UI's html/template files
locales/            UI translations: en, sv, nb
assets/             the UI's static files
docs/               this documentation set, plus guide/ which is served to editors
magefiles/          the mage targets
```

## Building and running

### Prerequisites

- Go 1.27.1+
- `libhunspell-dev` (CGo dependency)
- PostgreSQL

### Build

```bash
go build ./...
```

### Database setup

```bash
mage sql:db       # create database and role
mage sql:migrate  # run tern migrations
```

### Run

```bash
go run ./cmd/spell run
```

### Test

```bash
go test ./...
```

### Lint

```bash
# CI uses golangci-lint v2.13; the config is .golangci.yml
golangci-lint run --timeout=4m
```

## Configuration

All flags can also be set via environment variables.

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--addr` | `ADDR` | `:1080` | API server listen address |
| `--profile-addr` | `PROFILE_ADDR` | `:1081` | Debug/profiling server |
| `--tls-addr` | `TLS_ADDR` | `:1443` | TLS server listen address |
| `--cert-file` | `TLS_CERT` | | TLS certificate file |
| `--key-file` | `TLS_KEY` | | TLS key file |
| `--db` | `CONN_STRING` | `postgres://elephant-spell:pass@localhost/elephant-spell` | Primary database connection (used for LISTEN/NOTIFY) |
| `--db-bouncer` | `BOUNCER_CONN_STRING` | | Optional PgBouncer connection for regular queries |
| `--db-max-conns` | `DB_MAX_CONNS` | `8` | Size of the pool queries run on: the bouncer pool when `BOUNCER_CONN_STRING` is set, otherwise the direct pool. With a bouncer the direct pool is fixed at 2. Zero or less leaves it to pgx |
| `--log-level` | `LOG_LEVEL` | `debug` | Log level |
| `--cors-host` | `CORS_HOSTS` | | CORS hosts (supports wildcards) |
| `--oidc-provider` | `OIDC_PROVIDER` | | OIDC provider URL (required for web UI) |
| `--client-id` | `CLIENT_ID` | | OIDC client ID |
| `--client-secret` | `CLIENT_SECRET` | | OIDC client secret |
| `--callback-url` | `CALLBACK_URL` | `http://localhost:1080/auth/callback` | OIDC callback URL |
| `--default-language` | `DEFAULT_LANGUAGE` | `sv-se` | Language to redirect to from the root page |
| `--insecure-cookies` | `INSECURE_COOKIES` | `false` | Drop `Secure` from the session cookies, for serving the UI over plain HTTP locally |
| | `COOKIE_KEY_1`, `COOKIE_KEY_2`, ... | | Cookie keyring, required. See [Cookie keys](#cookie-keys) |

### Cookie keys

The web UI's session cookie is sealed with AES-256-GCM, and the service will
not start without at least one currently usable key. Each key is its own
environment variable — `COOKIE_KEY_1`, `COOKIE_KEY_2` and so on — holding an
RFC 3339 timestamp and the standard base64 of 32 random bytes, separated by an
underscore:

```
COOKIE_KEY_1=2026-08-01T00:00:00Z_TWFuIGlzIGRpc3Rpbmd1aXNoZWQsIG5vdCBvbmx5IGJ5IA==
```

The key sealed with is the one whose timestamp is the latest of those that have
passed; every configured key still opens, which is what makes a rollover a
matter of adding the next variable ahead of its use-after date and removing the
old one once no session can be sealed under it. `howdah.GenerateCookieKey`
produces a secret, and howdah's README carries the rotation runbook.

## RPC services

### `elephant.spell.Check` -- spellcheck text

**`Text`** checks text for spelling errors and custom dictionary matches:

``` json
POST twirp/elephant.spell.Check/Text

{
  "language": "sv-se",
  "text": [
    "Nu går vi till kriminalvårdsanstalten.",
    "En riktig relikt!",
    "Hette han Mohammar Gadaffi?",
    "Ska man ressa till Vitryssland?"
  ]
}
```

Response (using both the built-in Swedish dictionary and custom entries):

``` json
{
  "misspelled": [
    {
      "entries": [
        {
          "text": "kriminalvårdsanstalten",
          "level": "LEVEL_ERROR"
        }
      ]
    },
    {
      "entries": [
        {
          "text": "relikt",
          "level": "LEVEL_SUGGESTION"
        }
      ]
    },
    {
      "entries": [
        {
          "text": "Mohammar Gadaffi",
          "level": "LEVEL_ERROR"
        }
      ]
    },
    {
      "entries": [
        {
          "text": "Vitryssland",
          "level": "LEVEL_ERROR"
        },
        {
          "text": "ressa",
          "level": "LEVEL_ERROR"
        }
      ]
    }
  ]
}
```

**`Suggestions`** returns replacement suggestions for a misspelled word:

``` json
POST twirp/elephant.spell.Check/Suggestions

{
  "text": "ressa",
  "language": "sv-se"
}
```

``` json
{
  "suggestions": [
    { "text": "resas" },
    { "text": "resa" },
    { "text": "dressa" },
    { "text": "pressa" }
  ]
}
```

Custom dictionary entries provide targeted suggestions with descriptions:

``` json
POST twirp/elephant.spell.Check/Suggestions

{
  "text": "kriminalvårdsanstalten",
  "language": "sv-se"
}
```

``` json
{
  "suggestions": [
    {
      "text": "fängelset",
      "description": "Skriv fängelse och inte kriminalvårdsanstalt."
    }
  ]
}
```

### `elephant.spell.Dictionaries` -- manage custom entries

**`SetEntry`** adds or updates a custom dictionary entry:

``` json
POST twirp/elephant.spell.Dictionaries/SetEntry

{
  "entry": {
    "language": "sv-se",
    "text": "Belarus",
    "status": "approved",
    "description": "Vitryssland var det gamla namnet på Belarus",
    "common_mistakes": ["Vitryssland"]
  }
}
```

The custom dictionary can add previously unknown words and encourage the replacement of words that don't follow your language guidelines. Entries support:

- **`common_mistakes`**: words/phrases that should be flagged and replaced with this entry's text.
- **`forms`**: maps specific inflected mistakes to specific replacements (e.g. `"kriminalvårdsanstalten": "fängelset"`).
- **`level`**: `LEVEL_ERROR` (default) for corrections, `LEVEL_SUGGESTION` for softer recommendations.
- **Pattern expansion**: `{A|B} {1|2}` in common mistakes expands to all combinations, useful for names with many variant spellings.

Example entries showing these features:

``` json
{
  "entries": [
    {
      "language": "sv-se",
      "text": "fängelse",
      "status": "approved",
      "description": "Skriv fängelse och inte kriminalvårdsanstalt.",
      "common_mistakes": ["kriminalvårdsanstalt"],
      "level": "LEVEL_ERROR",
      "forms": {
        "kriminalvårdsanstalten": "fängelset",
        "kriminalvårdsanstalter": "fängelser"
      }
    },
    {
      "language": "sv-se",
      "text": "Muammar Gaddafi",
      "status": "approved",
      "common_mistakes": [
        "{Mohammar|Mohammer|Muammar|Muhammar|Muhammer} {Gadaffi|Ghadaffi|Ghadafi|Kadhaffi|Kadhafi|Khadaffi}"
      ],
      "level": "LEVEL_ERROR"
    },
    {
      "language": "sv-se",
      "text": "relik",
      "status": "approved",
      "description": "Relik har religiös betydelse. En kroppsdel eller ett föremål som vördas. Relikt är mer allmänt en kvarleva.",
      "common_mistakes": ["relikt"],
      "level": "LEVEL_SUGGESTION"
    }
  ]
}
```

## spell-client CLI

A command-line tool for managing custom dictionaries. Requires the `spell-client` environment to be configured with a `spell` endpoint and OIDC credentials (via `clitools`).

```bash
go build ./cmd/spell-client
```

### download

Download all entries for a language as newline-delimited JSON (one JSON object per line):

```bash
spell-client download --language sv-se > dict.ndjson
```

Each line is a protobuf-JSON serialised `CustomEntry`. Output goes to stdout so it can be piped or redirected.

### upload

Upload entries from a newline-delimited JSON file:

```bash
spell-client upload --file dict.ndjson
```

The language and all other fields are taken from each JSON object, so a single file can contain entries for multiple languages. Unknown fields are silently ignored, making it forward-compatible with newer entry schemas.

### upload-csv

Upload entries from a CSV file (columns: correct, mistakes, comment):

```bash
spell-client upload-csv --file entries.csv --language sv-se
```

### Global flags

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--env` | `ENV` | `stage` | Environment (local/stage/prod) |

## Web UI

The service includes a web UI for managing custom dictionary entries, available at the API server address (default `:1080`). It requires OIDC authentication and the `spell_write` scope for making changes.

## Supported languages

The following hunspell dictionaries are bundled:

- British English (`en-gb`)
- Danish (`da`)
- Finnish (`fi`)
- Norwegian Bokmal (`nb`)
- Norwegian Nynorsk (`nn`)
- Swedish (`sv`)
- US English (`en-us`)

## Docker

The included `Dockerfile` builds a minimal Debian image with the hunspell runtime library. Exposed ports: `1080` (API), `1081` (profiling), `1443` (TLS).

```bash
docker build -t elephant-spell .
docker run -e CONN_STRING=postgres://... -e OIDC_PROVIDER=... elephant-spell
```

## Pending work

**No metrics of its own.** The service registers no collectors beyond the pool statistics `main` wires up: everything else on `/metrics` comes from elephantine, the job lock, the FanOut recovery tracker and the Go runtime. Nothing counts spellchecks, nothing reports how many entries or rules a replica has loaded, and — the one that bites — **eventlog lag is not exported**, so "is this replica serving the current dictionary?" cannot be answered from monitoring. [`docs/observability.md`](docs/observability.md#what-is-missing) has the full list and what stands in for it meanwhile.

**No readiness check.** Nothing is registered with `AddReadyFunction` or `AddOptionalReadyFunction`, so a replica reports itself alive as soon as the HTTP listener is up — including while it is still paging through the startup preload. During a rollout that is a window in which a fresh replica answers checks against a partial dictionary. A check here wants `AddOptionalReadyFunction`, since a required one that touches the pool takes replicas out of service exactly when the pool is saturated.

**The Twirp mount is still carrying the traffic.** Both stacks are served, but nothing has moved onto Connect yet. `rpc_protocol_responses_total{protocol="twirp"}` going to zero for a method is what says its Twirp mount can be removed, and the `client_id` label names the callers that have to move first.

**Connect and Twirp spell JSON field names differently.** A Connect JSON response uses lowerCamelCase (`customOnly`) where Twirp uses the `.proto` spelling (`custom_only`). Generated clients are unaffected and requests are accepted either way; a caller that reads JSON by hand and changes only the path prefix gets a 200 and `undefined` for every multi-word field.

**No incident history.** The failure modes in [`docs/ops.md`](docs/ops.md#failure-modes) are read off the code rather than taken from a real incident, so they carry no measured numbers. The first one should be written in with them.
