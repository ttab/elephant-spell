# Rules are keyed by a sequential id, not by name

Pattern rules were originally keyed by `(language, name)`, which made the name a primary key: renaming a rule deleted one and created another, losing its moderation status, and two rules could not share a label however unrelated their patterns.

Rules now carry a generated sequential id as their primary key, and the name is a non-unique human-readable label that can be edited and duplicated freely. The `Rules` service distinguishes create from update by id and returns the assigned one; the eventlog carries the rule id; the rules and moderation UIs address rules by id.

**Consequence:** the eventlog's `entry` column carries the rule id as *text* for rule events, which is why `applyRuleEvent` parses it. That is the cost of one eventlog table serving two kinds of item, and it is cheaper than a second table and a second consumer.
