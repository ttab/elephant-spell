# Spell service guide

The spell service combines the built-in hunspell dictionaries with a custom,
editor-managed layer. That layer has two kinds of entity, each managed in its
own section of the admin UI:

- **[Dictionaries](/docs/dictionaries)** — words and phrases: corrections for
  misspellings and alternative spellings, inflected forms, and case handling.
- **[Rules](/docs/rules)** — token patterns that catch errors which can't be
  listed as fixed strings, such as number ranges, gaps between words, and
  context-dependent corrections.

## Moderation

Both words and rules carry a status, `accepted` or `pending`. Both are active in
the spellchecker regardless of status, but a correction based on a `pending`
item is flagged in the response so clients can mark it as unreviewed. The
**Moderation** queue lists pending words and rules together so the language desk
can work through them and accept or reject each one.
