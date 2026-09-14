# Rules live in their own table, not on the entry they were attached to

Pattern rules were first stored inside a dictionary entry's jsonb `data` column, as `EntryData.rule`. It made a rule a property of a word, which it is not: a rule about spacing around a number range belongs to no word, and the entry it had been parked on then could not be deleted without taking the rule with it.

Rules moved to a `rule` table of their own, managed by a separate `elephant.spell.Rules` service, with existing `data.rule` entries migrated across in `006_rules.sql`. The eventlog gained a `kind` column so one consumer can route word changes and rule changes to the right store.

**Consequence:** `kind` is what makes a single eventlog serve both, and an event whose `kind` is unrecognised is dropped rather than guessed at. A third kind of item is a schema change plus a branch in `applyEvent`, not a new log.
