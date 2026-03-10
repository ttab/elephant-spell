# Dictionary entries

Custom dictionary entries extend the built-in hunspell dictionaries with
organisation-specific words, phrases, and common mistake corrections.

## Entry fields

Each entry has the following fields:

| Field | Description |
|-------|-------------|
| **Text** | The correct word or phrase. |
| **Status** | `accepted` or `pending`. Only accepted entries are active in the spellchecker. |
| **Description** | Optional explanation shown to editors as context for the correction. |
| **Correction level** | `error` flags the text as wrong, `suggestion` offers a softer recommendation. |
| **Common mistakes** | Misspellings or alternative spellings that should be corrected to the entry text. |
| **Forms** | Maps specific incorrect inflections to specific correct replacements. |

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
