# Spell service guide

The spell service combines the built-in hunspell dictionaries with a custom,
editor-managed layer. That layer has two kinds of entity, each managed in its
own section of the admin UI:

- **[Dictionaries](dictionaries.md)** — words and phrases: corrections for
  misspellings and alternative spellings, inflected forms, and case handling.
- **[Rules](rules.md)** — patterns with placeholders that catch errors which
  can't be listed as fixed strings, such as number ranges, gaps between words,
  and context-dependent corrections.

## What is never flagged

Phone numbers (`0720-349665`, `+46 8 692 27 00`, `08/692 27 00`), email
addresses and web addresses (`https://…`, `www.tt.se`, `forms.gle/abc`,
`tt.se`) are left alone: neither hunspell, the dictionaries nor the rules see
them. A number range such as `12-15` or `08-12` is still a number range, so a
rule on `{digit}-{digit}` keeps working there, and a word that mixes letters
and digits, like `2o24`, is still checked.

## Moderation

Both words and rules carry a status, `accepted` or `pending`. Both are active in
the spellchecker regardless of status, but a correction based on a `pending`
item is flagged in the response so clients can mark it as unreviewed. The
**Moderation** queue lists pending words and rules together so the language desk
can work through them and accept or reject each one.
