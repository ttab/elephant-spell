# Spell

The spellcheck service: hunspell's language dictionaries plus an editor-managed layer of corrections, phrases and pattern rules, served over RPC and edited through an admin UI.

This glossary binds prose — documents, issue titles, test names, commit messages. It does not bind shipped identifiers; where a table or RPC name disagrees with the term chosen here, that is recorded under [Known exceptions](#known-exceptions) rather than treated as a rename candidate.

## Upstream vocabulary

**This service has no upstream elephant glossary.** It imports `elephant-api/spell` — its own API declarations — and nothing else from another service. It does not read the repository's documents and is not a consumer of the platform's eventlog, so none of [`elephant-repository`'s vocabulary](https://github.com/ttab/elephant-repository/blob/main/CONTEXT.md) applies here. Do not reach for it on the assumption that every elephant service speaks it.

`howdah` owns the session and cookie-keyring vocabulary this service's configuration is written in; read [its README](https://github.com/ttab/howdah) when those words matter. `elephantine`, `eltest`, `mage` and `clitools` are framework and tooling dependencies and own no domain vocabulary.

**hunspell** owns affix file, dictionary file, stem and suggestion. This service uses those words in hunspell's sense and does not redefine them.

## Language

**Entry**:
One item in the custom dictionary: a word or phrase, its correction level, its common mistakes, its alternate forms and its guards. Keyed by `(language, entry)`.
_Avoid_: word, term, dictionary item

**Custom dictionary**:
The editor-managed layer, as opposed to the hunspell dictionaries that ship in the binary. Always qualify which one is meant when both are in play.
_Avoid_: user dictionary, local dictionary

**Common mistake**:
A misspelling recorded on an entry so that it is flagged and corrected to the entry's text. Written with `{A|B}` expansion, which generates every combination.
_Avoid_: typo, misspelling (as a noun for the stored thing), error string

**Form**:
An alternate inflection of an entry that is also accepted.
_Avoid_: variant, inflection (in prose; the code says `Forms`)

**Guard**:
A context condition on an entry or rule — `before`, `after`, `not_before`, `not_after` — evaluated against the neighbouring word after the match has been located. A guard suppresses or requires a match; it never creates one.
_Avoid_: condition, context rule, filter

**Rule**:
A pattern rule: a pattern with `{digit}`, `{word}` and `{gap(N)}` placeholders, a replacement template, and optional guards. Keyed by a generated sequential id. Distinct from an entry, in its own table and its own RPC service.
_Avoid_: pattern entry, regex rule, token rule

**Pattern**:
The matching half of a rule. Compiles to an RE2 regex in which everything but a placeholder — whitespace included — is literal.
_Avoid_: expression, matcher

**Expansion**:
One of the strings a `{A|B}` common mistake generates. "Three expansions" means three generated strings, not three braces.
_Avoid_: permutation, combination, variant

**Correction level**:
Whether a match is reported as an error or as a suggestion. The API spells it `CorrectionLevel`; the UI and the templates spell the values `error` and `suggestion`.
_Avoid_: severity, priority

**Status**:
An entry's or rule's moderation state — `pending` until the quality desk accepts it, `approved` after. **An entry is used for spellchecking regardless of status**; the status is surfaced on the correction so a client can flag one that came from an unreviewed entry.
_Avoid_: state, review state, approval

**Moderation**:
The quality desk's review of pending entries and rules, done in the admin UI's moderation queue. Accepting keeps an item; rejecting deletes it.
_Avoid_: approval workflow, review queue (for the activity; the queue is the UI)

**Eventlog**:
This service's internal change feed: a Postgres table carrying `(language, entry, deleted, kind)` so each replica can bring its in-memory tries in line. **Not the platform's eventlog** — see [False friends](#false-friends).
_Avoid_: change log, feed, journal

**Cursor**:
The highest eventlog id a replica has applied. Held in memory only; never persisted, because a restart reloads full state instead.
_Avoid_: offset, position, watermark

**Drain**:
Reading and applying every event past the cursor until the log is exhausted. A drain is one operation however many batches it takes.
_Avoid_: poll, sync, catch-up (as a noun)

**Preload**:
The startup pass that loads every current entry and rule from the database as the baseline the eventlog is then replayed onto.
_Avoid_: bootstrap, initial load, warm-up

**Spellchecker**:
The per-language `*Spellcheck`: one hunspell instance, four tries, the compiled rules, and a buffer pool. One per supported language, per replica.
_Avoid_: checker (which is the hunspell binding), engine

**Phrase**:
A sequence of up to three words matched as a unit by the sliding window. Every entry is a phrase as far as the tries are concerned, whether or not it contains a space.
_Avoid_: n-gram, token sequence

**Stack**:
Which RPC protocol a caller is speaking — Twirp or Connect. Both are served from the same implementation on different paths. Say "the Connect stack", not "the Connect API": the API is the same one.
_Avoid_: protocol, transport, mount (for the protocol; a mount is the registration)

## False friends

**`eventlog` means something different here than it does in the platform.** elephant-repository's eventlog is the durable source of truth from which state is rebuilt. This service's eventlog is a transport: it is pruned to one hour, nothing is ever recovered from it, and a restarting replica ignores it entirely and reloads from the `entry` and `rule` tables. A document that says "the eventlog" without saying whose has to be fixed.

**`status` is moderation state, not workflow status.** The platform's status is a named pointer to a document version. Here it is whether the quality desk has looked at an entry yet.

**`scope` is an OAuth2 scope in the ordinary sense** — `spell_write` — and carries none of the platform's document-permission machinery.

## Known exceptions

Shipped identifiers that disagree with the terms above, recorded rather than renamed:

| Identifier | Term | Why it stays |
|---|---|---|
| `eventlog` table and channel | Eventlog, in the local sense | Renaming the channel would need every replica restarted in lockstep to avoid a window where publishers and subscribers disagree. |
| `entry` column on `eventlog` | Holds the *entry text* for word events and the **rule id as text** for rule events | A consequence of one log serving two kinds of item; see [ADR-0002](docs/adr/0002-rules-keyed-by-id.md). |
| `ListEntriesRequest.query` | Was `prefix` until v1.5.0, when the search stopped being prefix-only | Already renamed once; the current name is correct. |
| `CustomEntry.case_sensitive` | Entry, case sensitivity | Field on the shipped protobuf message. |
