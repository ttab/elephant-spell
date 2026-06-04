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

A pattern is matched against the text directly. Placeholders in curly braces
capture parts of the match; everything else is literal text, matched
case-insensitively.

| Placeholder | Matches |
|-------------|---------|
| `{digit}` | a run of digits (captured) |
| `{word}` | a run of letters (captured) |
| `{gap}` | up to 4 whitespace-separated words in between (captured) |
| `{gap(N)}` | up to N words in between (captured) |

Captures are referenced in the **replacement** by position — `{1}` is the first
capture, `{2}` the second, and so on.

### Whitespace is significant

Because literal text (including spaces) is matched as written, you control
spacing exactly. A run of spaces in the pattern means "one or more whitespace
characters"; adjacency means none:

| Pattern | Matches | Does not match |
|---------|---------|----------------|
| `{digit}-{digit}` | `12-15` | `12 - 15` |
| `{digit} - {digit}` | `12 - 15` | `12-15` |
| `{digit}kr` | `5kr` | `5 kr` |
| `{digit} kr` | `5 kr` | `5kr` |

A `{digit}`/`{word}` next to literal text only matches at a word boundary, so
`{digit}kr` matches `5kr` but not the `5kr` inside `5krona`.

## Examples

| Pattern | Replacement | Effect |
|---------|-------------|--------|
| `{digit}-{digit}` | `{1}–{2}` | `12-15` → `12–15` (en dash) |
| `{digit} kr` | `{1} kronor` | `5 kr` → `5 kronor` |
| `inte {gap} varken` | `inte {1}` | `inte längre varken` → `inte längre` |

The rule editor has a **sample input** field with a **Test** button — enter some
text and the matches and their suggested replacements are shown beneath it.

## Context guards

A rule can be limited by the words next to a match:

- **skip if preceded / followed by** — suppress the match when the neighbouring
  word is one of these (e.g. don't flag "alltför" when followed by "att").
- **only if preceded / followed by** — match only when the neighbouring word is
  one of these.

Guards are evaluated against the immediately adjacent word and are how an
ambiguous correction is kept from firing in the contexts where it is actually
correct.
