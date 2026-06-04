# Pattern rules

A **pattern rule** matches a sequence of *tokens* rather than literal text. This
catches errors that can't be enumerated as fixed strings — number ranges, words
with other words in between, and corrections that depend on the surrounding
words. For plain words, phrases and inflected forms see
[Dictionaries](/docs/dictionaries).

Rules are managed in the **Rules** section of the admin UI, separately from
dictionary words.

## Rule fields

| Field | Description |
|-------|-------------|
| **Name** | Identifies the rule (its key, together with the language). |
| **Status** | `accepted` or `pending`, like dictionary words. Both are active; pending rules are flagged and worked through in the moderation queue. |
| **Description** | Optional explanation shown to editors as context for the correction. |
| **Correction level** | `error` flags a match as wrong, `suggestion` offers a softer recommendation. |
| **Pattern** | The token pattern to match (see the DSL below). |
| **Replacement** | The suggested correction, referencing captured tokens as `{1}`, `{2}`, … |
| **Context guards** | Optional limits on the words next to a match (see below). |

## Pattern DSL

A pattern is a space-separated sequence of tokens:

| Token | Matches |
|-------|---------|
| a literal (e.g. `kr`, `-`) | that exact token, ignoring case |
| `:digit` | a run of digits (captured) |
| `:word` | a single word (captured) |
| `:gap` | up to 4 intervening words (captured) |
| `:gap(N)` | up to N intervening words (captured) |

Capturing tokens are referenced in the **replacement** by position — `{1}` is
the first capture, `{2}` the second, and so on.

## Examples

| Pattern | Replacement | "12-15" / "5 kr" / "inte längre varken" → |
|---------|-------------|-------------------------------------------|
| `:digit - :digit` | `{1}–{2}` | "12-15" → "12–15" (en dash) |
| `:digit kr` | `{1} kronor` | "5 kr" → "5 kronor" |
| `inte :gap varken` | `inte {1}` | "inte längre varken" → "inte längre" |

The number-range dash rule (`:digit - :digit`) is built in and always active.

## Context guards

A rule can be limited by the words next to a match:

- **skip if preceded / followed by** — suppress the match when the neighbouring
  word is one of these (e.g. don't flag "alltför" when followed by "att").
- **only if preceded / followed by** — match only when the neighbouring word is
  one of these.

Guards are evaluated against the immediately adjacent word and are how an
ambiguous correction is kept from firing in the contexts where it is actually
correct.
