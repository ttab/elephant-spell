# Dictionary words

Dictionary words extend the built-in hunspell dictionaries with
organisation-specific words, phrases, and common-mistake corrections. For
patterns (number ranges, gaps between words, context-dependent corrections) see
[Rules](/docs/rules).

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
| **Case sensitive** | When off (the default), the text and common mistakes match regardless of casing, and suggestions take on the leading-capital style of the matched word — so a lowercase entry is still caught at the start of a sentence. Enable it for proper nouns that must only match their exact casing. A case-sensitive entry also corrects its own *miscasings*: any leading-letter variant of the text or a common mistake (e.g. "mexico city", "Mexico city") is flagged and suggested back to the exact casing ("Mexico City"). |
| **Context guards** | Optional limits on the words next to a match (see below). |

## Context guards

An entry can be limited by the words immediately next to a match, the same way
[pattern rules](/docs/rules#context-guards) are:

- **skip if preceded / followed by** — suppress the match when the neighbouring
  word is one of these.
- **only if preceded / followed by** — match only when the neighbouring word is
  one of these.

This keeps simple word-with-context corrections in the dictionary instead of
needing a pattern rule. Guards respect the entry's case sensitivity, and because
the match has already been located they cost nothing extra to evaluate.

### Example: Mexiko (the country) vs Mexico City (the city)

In Swedish the country is spelled **Mexiko**, but the city keeps its English
form **Mexico City**. Two case-sensitive entries handle both without stepping on
each other:

**Entry 1 — the country** (text `Mexiko`)

| Field | Value |
|---|---|
| Common mistakes | `Mexico` |
| Skip if followed by | `City`, `Citys` |

A stray "Mexico" is corrected to "Mexiko" — *except* when followed by "City" or
"Citys", where "Mexico" is correct because it is part of the city name.

**Entry 2 — the city** (text `Mexico City`)

| Field | Value |
|---|---|
| Common mistakes | `Mexiko City` |
| Forms | `Mexiko Citys` → `Mexico Citys` |

This corrects an over-Swedishified "Mexiko City" back to "Mexico City".

Putting it together:

| Input | Result |
|---|---|
| `Mexico` | → `Mexiko` (country) |
| `Mexico City` | left alone — the *skip if followed by* guard suppresses the country correction |
| `Mexiko City` | → `Mexico City` (the city entry's `Mexiko City` common mistake) |
| `mexiko` | → `Mexiko` (case-sensitive entries still correct leading-letter miscasings) |

Both entries are case-sensitive, so "Mexico"/"Mexiko" only match as the proper
nouns — and because they are case-sensitive they also flag miscasings like
"mexiko" and suggest the exact casing.

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

In the entry editor the common-mistakes field highlights the `{A|B}` syntax and
shows a live count of how many combinations each line expands to. Click that
count to open a list of every expansion.

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
to its correct counterpart. In the entry editor each mapping is a row with the
incorrect form on the left and its correct replacement on the right:

| Incorrect | → | Correct |
|---|---|---|
| kriminalvårdsanstalten | → | fängelset |
| kriminalvårdsanstalter | → | fängelser |

When the spellchecker encounters "kriminalvårdsanstalten" it will suggest
"fängelset" (not just the base form "fängelse"), giving editors a
grammatically correct replacement.

The base form "kriminalvårdsanstalt" is listed in common mistakes and maps
to the entry text "fängelse" as usual.
