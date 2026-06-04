# Dictionary entries

Custom dictionary entries extend the built-in hunspell dictionaries with
organisation-specific words, phrases, and common mistake corrections.

## Entry fields

Each entry has the following fields:

| Field | Description |
|-------|-------------|
| **Text** | The correct word or phrase. |
| **Status** | `accepted` or `pending`. Both are active in the spellchecker; corrections based on a `pending` entry are flagged as such in the response so clients can mark them as unreviewed. Pending entries are worked through in the moderation queue. |
| **Description** | Optional explanation shown to editors as context for the correction. |
| **Correction level** | `error` flags the text as wrong, `suggestion` offers a softer recommendation. |
| **Common mistakes** | Misspellings or alternative spellings that should be corrected to the entry text. |
| **Forms** | Maps specific incorrect inflections to specific correct replacements. |
| **Case sensitive** | When off (the default), the text and common mistakes match regardless of casing, and suggestions take on the leading-capital style of the matched word — so a lowercase entry is still caught at the start of a sentence. Enable it for proper nouns that must only match their exact casing. |

## Common mistakes and pattern expansion

The common mistakes field lists words or phrases that the spellchecker should
flag and suggest replacing with the entry's correct text.

Patterns use `{A|B|C}` syntax to generate all combinations of alternative
spellings. This is especially useful for names that have many transliterated
variants.

### Example: Muammar Gaddafi

The name "Muammar Gaddafi" has dozens of different transliterations in Western
media. Instead of listing every combination manually, the entry uses expansion
patterns:

**Entry text:** `Muammar Gaddafi`

**Common mistakes pattern:**

```
{Mohammar|Mohammer|Muammar|Muhammar|Muhammer} {Gadaffi|Ghadaffi|Ghadafi|Kadhaffi|Kadhafi|Khadaffi}
```

This single pattern expands to **30 combinations** (5 first-name variants
times 6 last-name variants):

- Mohammar Gadaffi, Mohammar Ghadaffi, Mohammar Ghadafi, ...
- Mohammer Gadaffi, Mohammer Ghadaffi, ...
- Muammar Gadaffi, Muammar Ghadaffi, ...
- ...and so on.

Any of these misspellings will be flagged and the spellchecker will suggest
"Muammar Gaddafi" as the correction.

Multiple expansion groups can be combined with literal text:

```
Khan {Younis|Yunes}
```

This expands to "Khan Younis" and "Khan Yunes".

A pattern without any `{...}` groups is treated as a literal string and used
as-is.

## Forms

While common mistakes map many misspellings to the same correction, **forms**
let you map specific incorrect inflections to specific correct replacements.

This is useful for Swedish (and other inflected languages) where a word changes
form depending on its grammatical role, and each incorrect form has a specific
correct counterpart.

### Example: fängelse (prison)

The word "kriminalvårdsanstalt" should be replaced with "fängelse" in Swedish
editorial text. But the replacement depends on the inflected form:

| Incorrect form | Correct replacement |
|---|---|
| kriminalvårdsanstalten | fängelset |
| kriminalvårdsanstalter | fängelser |

In the entry for "fängelse", the **forms** field maps each incorrect inflection
to its correct counterpart:

```
kriminalvårdsanstalten=fängelset
kriminalvårdsanstalter=fängelser
```

When the spellchecker encounters "kriminalvårdsanstalten" it will suggest
"fängelset" (not just the base form "fängelse"), giving editors a
grammatically correct replacement.

The base form "kriminalvårdsanstalt" is listed in common mistakes and maps
to the entry text "fängelse" as usual.

## Pattern rules

A **pattern rule** matches a sequence of *tokens* rather than literal text. This
catches errors that can't be enumerated as fixed strings — number ranges, words
with other words in between, and corrections that depend on the surrounding
words.

Enable "Treat this entry as a pattern rule" on an entry to turn it into a rule.

### Pattern DSL

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

### Examples

| Pattern | Replacement | "12-15" / "5 kr" / "inte längre varken" → |
|---------|-------------|-------------------------------------------|
| `:digit - :digit` | `{1}–{2}` | "12-15" → "12–15" (en dash) |
| `:digit kr` | `{1} kronor` | "5 kr" → "5 kronor" |
| `inte :gap varken` | `inte {1}` | "inte längre varken" → "inte längre" |

The number-range dash rule (`:digit - :digit`) is built in and always active.

### Context guards

A rule can be limited by the words next to a match:

- **skip if preceded / followed by** — suppress the match when the neighbouring
  word is one of these (e.g. don't flag "alltför" when followed by "att").
- **only if preceded / followed by** — match only when the neighbouring word is
  one of these.

Guards are evaluated against the immediately adjacent word and are how an
ambiguous correction is kept from firing in the contexts where it is actually
correct.
