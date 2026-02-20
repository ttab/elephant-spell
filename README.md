# Elephant spell

Spellcheck microservice combining [hunspell](https://hunspell.github.io/) with a custom dictionary stored in PostgreSQL. Exposes two [Twirp](https://github.com/twitchtv/twirp) RPC services and a web UI for dictionary management.

## Architecture

- **Dual database pools**: a direct connection (`CONN_STRING`) for PostgreSQL `LISTEN/NOTIFY` and an optional PgBouncer connection (`BOUNCER_CONN_STRING`) for general queries. LISTEN/NOTIFY cannot go through PgBouncer.
- **Real-time updates**: custom dictionary changes propagate instantly via PostgreSQL `LISTEN/NOTIFY` on the `entry_update` channel.
- **Phrase matching**: sliding window (up to 3 words) over text using tries for both valid phrases and common mistakes. Pattern expansion syntax `{A|B} {1|2}` generates all combinations.
- **Embedded dictionaries**: hunspell dictionaries for all supported languages are bundled in the binary via `//go:embed`.

## Building and running

### Prerequisites

- Go 1.25+
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
# CI uses golangci-lint v2.7
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
| `--log-level` | `LOG_LEVEL` | `debug` | Log level |
| `--cors-host` | `CORS_HOSTS` | | CORS hosts (supports wildcards) |
| `--oidc-provider` | `OIDC_PROVIDER` | | OIDC provider URL (required for web UI) |
| `--client-id` | `CLIENT_ID` | | OIDC client ID |
| `--client-secret` | `CLIENT_SECRET` | | OIDC client secret |
| `--callback-url` | `CALLBACK_URL` | `http://localhost:1080/auth/callback` | OIDC callback URL |

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
