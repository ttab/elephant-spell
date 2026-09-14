# Pattern rules compile to a regex, not to a token match

Rules were first matched by running the Unicode word segmenter over the text and comparing the resulting tokens against a token pattern. That made whitespace invisible and inconsistently so — `5kr` never split into `5` and `kr`, so a rule meant to catch a missing space could not — and the segmenter's idea of a token boundary was not something a rule author could predict or see.

Patterns now compile to a safe RE2 regex. `{digit}`, `{word}` and `{gap(N)}` are the only placeholders; **everything else, whitespace included, is matched literally and case-insensitively**, so `{digit}-{digit}` matches `12-15` and not `12 - 15`. The author sees exactly what they wrote.

**Do not reintroduce segmentation ahead of pattern matching.** A matcher that tokenises first cannot express a rule about spacing, which is a large share of what the rules are for. The phrase tries are a separate path and legitimately do run the segmenter — they match words, not spacing — so the presence of `PhraseIterator` in the same package is not a precedent for putting one back here.
