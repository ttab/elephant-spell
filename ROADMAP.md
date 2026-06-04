# ROADMAP

This roadmap is derived from editorial feedback on the spell service, plus one
foundational piece of work (item 1) that the quality desk needs before the rest.
Most of the editorial items stem from the same root cause: **the matching
engine only does exact, case-sensitive, gap-free, context-free string lookups
over a 3-word window.**

The items are ordered from first/easiest to hardest. The later items share a
common architectural need, so this document first describes the per-item work
and then the unifying changes that make items 4–6 tractable instead of a pile of
special cases.

## Background: how matching works today

- One `*Spellcheck` per language (`internal/spellcheck.go`) holds a hunspell
  instance plus two tries: `trie` (valid phrases, keyed by exact text) and
  `mistakeTrie` (common mistakes → `*Phrase`).
- `Check.Text` receives `TextRequest.Text` as a `[]string` and checks each
  independently. Each string is a UI-scoped chunk — a paragraph, headline, or
  similar — so matching and context already operate within a small, naturally
  bounded scope, not a whole article.
- `Check()` runs `PhraseIterator` (`internal/phrases.go`), a sliding window of
  up to 3 contiguous tokens. Each window is concatenated into a string and
  looked up in the tries. Matches are **exact and case-sensitive**.
- `Expand()` (`internal/expand.go`) turns `{A|B} {1|2}` patterns into the full
  cartesian product of literal strings, which are then inserted into the trie.
  This is enumeration, not pattern matching.
- `Forms` (in `EntryData`, `postgres/entry.go`) maps specific incorrect
  inflections to specific replacements.
- Entries carry a `status` (`pending`/`accepted`), but it is **inert today**:
  `preloadEntries` (`internal/service_entry_updates.go`) loads *all* entries
  regardless of status, so pending entries are silently active, and the status
  is not surfaced to clients. (`docs/dictionary-entries.md` even claims only
  accepted entries are active — the code disagrees.)
- Results (`spell.Misspelled` / `MisspelledEntry`) are keyed **only by the
  matched text**. That is fine for the editorial workflow: the client underlines
  the flagged span, the user right-clicks, the client calls the `Suggestions`
  RPC for that span, and the user picks a replacement manually — so positions /
  offsets are never needed, and repeated phrases are not a problem.

The matching model is the crux for the editorial items: an exact, case-sensitive,
contiguous-only match cannot express number ranges, gaps between words, case
folding, or "wrong in this context but fine in that one".

## Summary

| # | Item | Difficulty | Primary change |
|---|------|-----------|----------------|
| 1 | Accepted/pending status + moderation queue | Medium | Surface status in responses; new moderation UI |
| 2 | Capitalization handling | Easy | Matching layer (case folding) + per-entry case flag |
| 3 | Broader dictionary search (low priority) | Easy | Widen the UI search query in SQL |
| 4 | Regex rules (e.g. `digit–digit` dash) | Medium | New rule type → rule engine foundation |
| 5 | Context exceptions (`alltför` but not before `att`) | Medium–High | Context guards evaluated at Check time |
| 6 | Mistakes with words in the middle | High | Gap/token matching in the rule engine |

Items 4–6 are progressively richer uses of the same **token-based rule engine**
described in [Unifying architecture](#unifying-architecture). Build that
foundation incrementally with item 4, and 5–6 become configuration on top of it
rather than new subsystems. Items 1–3 are independent of that engine and can
ship first. (The editor's question about words with several senses needs no
roadmap item — see
[Addressed without new work](#addressed-without-new-work-words-with-several-senses-flera-betydelser).)

---

## 1. Accepted/pending status support + moderation queue

**Goal.** Make the `status` field real: keep using every entry for spellcheck
regardless of status, but **flag the status in the `Check` response** so clients
can tell the user a correction comes from an unreviewed (pending) entry; and add
a **moderation page** where the language editors / quality desk work through the
pending queue with simple accept/reject actions.

**Problem.** Today `status` is stored but inert. Pending entries are already
active in spellcheck (because `preloadEntries` applies no status filter), but:
(a) clients have no signal that a correction is unreviewed, and (b) there is no
workflow to review the queue. The behaviour and `docs/dictionary-entries.md`
also disagree about whether pending entries are active.

**Decisions / semantics.**

- **Spellcheck uses all entries regardless of status** — keep current
  behaviour, but make it intentional and documented. `pending` entries are
  active *and* flagged.
- **Accept** → set status to `accepted`.
- **Reject** → **delete the entry**. Because spellcheck deliberately ignores
  status, a lingering `rejected` status would stay active; deletion is the clean
  removal and the eventlog already records deletes. (Alternative, if an audit
  trail is wanted: a `rejected` status that is excluded at load time — but that
  reintroduces status-based filtering and contradicts "use regardless of
  status". Recommend delete; revisit only if an audit trail becomes a
  requirement.)

**Plan.**

1. **Engine — carry status through to results.**
   - Add `Status string` to `Phrase` (`internal/spellcheck.go`); set it in
     `entryAsPhrase` (`internal/service_entry_updates.go`).
   - In `Check()`, populate the status on each `MisspelledEntry` emitted from a
     custom phrase. Hunspell-only matches have no custom entry → empty status.
2. **Proto (`github.com/ttab/elephant-api`).**
   - Add `status` to `MisspelledEntry` so clients can render a "based on a
     pending entry" affordance. (Free-text string mirrors storage; an enum is
     optional.)
   - Add an optional `page_size` to `ListEntriesRequest` so the moderation view
     can page at ~10. `ListEntries` in `internal/service.go` currently
     hard-codes `limit = 100`; default `page_size` to 100 to preserve existing
     behaviour.
   - Add a lightweight `SetEntryStatus` RPC (`language`, `text`, `status`)
     behind the `spell_write` scope, so accept doesn't have to round-trip and
     re-submit the whole entry (which would race with concurrent edits). Reject
     reuses the existing `DeleteEntry`.
3. **Moderation UI (new howdah component or routes on `DictionariesUI`).**
   - **Per-language queue.** Route e.g. `GET /moderation/{language}/` (plus an
     htmx partial route for paging), listing entries via
     `ListEntries(status="pending", page_size=10)`. Pagination reuses the
     existing `Page`/`HasMore` pattern in `internal/ui_dictionaries.go`. Default
     to the editor's preferred language (reuse the `lang` cookie /
     `matchLanguage` logic already in `listPage`).
   - **Cross-language visibility + quick switch.** Show a language selector at
     the top of the page where each language carries a **pending-count badge**,
     so an editor moderating `sv-se` can see at a glance that, say, `en-gb` has
     items waiting and click straight over. This needs per-language *pending*
     counts (the current `ListDictionaries` returns only total counts): either
     extend `ListDictionaries`/`CustomDictionary` with a `pending_count`, or add
     a small `CountPendingByLanguage` query grouping
     `WHERE status = 'pending'` by language. Fetch all counts once for the
     selector; languages with zero pending render without a badge.
   - **Compact card layout**, one card per pending entry, showing all relevant
     fields: text, level (error/suggestion), description, common mistakes,
     forms, and `updated`/`updated_by`. Each card has **Accept** and **Reject**
     buttons (htmx POST; on success the card is removed from the list, the
     current language's pending count decrements, and the badge updates).
   - Add a `MenuHook` entry ("Moderation") and templates `moderation.html`
     (page + language selector + list) and `moderation_card.html` (card partial
     for htmx swap), alongside the existing templates in `templates/`.
   - Gate accept/reject behind `spell_write` (reuse `hasWriteScope`); a
     read-only viewer can see the queue but not act.
4. **Sync.** Accept (status change) and reject (delete) both flow through the
   existing `recordEntryChange` → eventlog → `applyEvent` →
   `AddPhrase`/`RemovePhrase` path, so live spellcheckers update automatically.
   For accept, `applyEvent` re-reads the entry and `AddPhrase` refreshes
   `Phrase.Status`; no new sync machinery needed.
5. **Docs.** Reconcile `docs/dictionary-entries.md`: state that both pending and
   accepted entries are active, that pending corrections are flagged to clients,
   and describe the moderation workflow.

**Notes / why first.** This is a quality-control capability the desk needs now,
it is independent of the matching-engine rework, and it makes `status` a
meaningful field — which every later item's entries also carry. Effort is
**medium**, mostly UI plus a small proto/RPC addition.

---

## 2. Capitalization handling (versalhantering)

> "Jag har lagt in allt (som inte är egennamn) helgement, men det innebär att
> ordet/uttrycket inte fångas upp när det står först i en mening och ska vara
> versalgement."

**Problem.** Entries are stored lowercase, but trie lookups are
case-sensitive, so `alltför` is not matched at the start of a sentence
(`Alltför`). Proper nouns must stay case-sensitive (`mexiko` ≠ `Mexiko`).

**Plan.**

1. Add a per-entry **case policy** to `EntryData`
   (`postgres/entry.go`) and `CustomEntry` (proto), e.g.
   `case_sensitive bool` (default `false` for ordinary words, `true` for proper
   nouns). No SQL migration needed — `data` is already `jsonb`.
2. In `Spellcheck.AddPhrase`, for case-insensitive entries also index a
   case-folded key. Keep the original casing on the `*Phrase` so suggestions
   are emitted in the canonical form.
3. In `Check()`, look up both the verbatim window and its case-folded form.
   When a match comes from the folded key, preserve the input's leading-capital
   style in the suggestion if appropriate (sentence-initial → capitalize the
   suggestion).
4. UI (`internal/ui_dictionaries.go`) + docs
   (`docs/dictionary-entries.md`): expose the case-sensitivity toggle and
   default new entries to case-insensitive.

**Notes / risks.** Swedish casing is locale-sensitive — use case folding, not
naive ASCII lowercasing. This item is self-contained and a good warm-up; it
does **not** require the rule engine, but the per-entry case flag should be
designed so the rule engine (items 4–6) reads the same policy.

---

## 3. Broader dictionary search (bredare sökning) — low priority

> "Vore bra med en bredare sökning."

**Problem.** The dictionary management web UI searches entries by **entry-text
prefix only**. The search box maps to `ListEntriesRequest.Prefix`
(`internal/ui_dictionaries.go`), which becomes `entry LIKE 'prefix%'` in the
`ListEntries` query (`postgres/queries.sql`). An editor looking for the entry
that handles a particular misspelling, or searching by the description text,
gets no hits unless they happen to type the start of the canonical entry text.
The likely ask is for the search to also match **common mistakes** and the
**description**.

**Plan.**

1. Widen the `ListEntries` query: in addition to `entry LIKE @pattern`, match
   `description ILIKE` and the `common_mistakes` array (e.g.
   `array_to_string(common_mistakes, ' ') ILIKE @pattern`, or an `EXISTS`
   over `unnest`). Combine with `OR`. Regenerate with `mage sql:generate`.
2. Shift semantics from prefix (`prefix%`) to substring (`%query%`) so matches
   inside mistakes/descriptions are found, and rename the parameter/field from
   `Prefix` to something like `Query`/`Search`
   (`ListEntriesRequest` in `github.com/ttab/elephant-api`, plus the UI query
   param at `internal/ui_dictionaries.go`). Keep the existing
   `varchar_pattern_ops` index in mind — a leading-`%` substring search won't
   use it, but the table is small enough that this is unlikely to matter; verify
   if it grows.
3. Update the UI label/placeholder to reflect that search now spans text,
   common mistakes, and description.

**Notes.** Self-contained and low risk — one SQL query, one proto field rename,
one UI tweak. Marked **low priority** by the editor. No relationship to the
matching engine; can be picked up independently whenever convenient.

---

## 4. Regex rules — e.g. `digit–digit` dash (tankstreck)

> "Det vore toppen med någon form av regex-hantering, exempelvis för att fånga
> upp [siffra]–[siffra] – alltså tankstreck i stället för bindestreck."

**Problem.** Number ranges (`12-15` → `12–15`) cannot be enumerated into the
trie — the set is unbounded. The matcher has no concept of character classes or
patterns.

**Plan — this item introduces the rule engine (see
[Unifying architecture](#unifying-architecture)).**

1. Introduce a new entry kind: a **rule** with a typed pattern rather than a
   literal mistake. Start with a deliberately small, safe pattern vocabulary
   (not raw user-supplied regex) — e.g. token classes `digit`, `word`,
   `space`, plus literals — so rules are bounded, reviewable, and can't cause
   catastrophic backtracking.
2. Represent the dash rule as: `digit`, literal `-`, `digit` ⇒ replace `-` with
   `–`. The replacement is a transformation, not a fixed string.
3. Run rules over the **token stream** produced once per `Check()` call (see the
   engine design below). `Check` flags the matched span (it becomes the entry's
   `Text`); the suggestion itself is produced by the rule's template — inline if
   `Check` was asked for suggestions, and again on demand when the client makes
   the `Suggestions` call on right-click (see
   [Generating suggestions](#generating-suggestions)).
4. Storage: add a `rule` payload to `EntryData` (jsonb) — type, pattern, and
   replacement template. UI + docs grow a "rule" entry editor.

**Notes / risks.** Keep the pattern language curated. If raw regex is ever
exposed, compile once at load, bound it with `regexp` (RE2, no backtracking),
and validate on write. The dash case is common enough to ship as a built-in
rule even before a general editor exists.

---

## 5. Context exceptions (kontextberoende undantag)

> "'alltför' ska skrivas ihop men inte om nästa ord är 'att'. Eller 'Mexiko'
> ska skrivas med k men inte om det efterföljs av 'City'."

**Problem.** A correction is valid only in some contexts. The current model has
no notion of surrounding tokens.

**Plan.**

1. Add **context guards** to entries/rules: optional `not_before`, `not_after`,
   `before`, `after` token conditions (literal or token-class), evaluated
   against neighbours in the token stream **at `Check` time**, where the full
   surrounding text is available.
   - `alltför` → suppress when `not_before: ["att"]`.
   - `Mexiko` (as a "use k" correction) → suppress when `not_after: ["City"]`.
2. Evaluate guards in the rule engine after a candidate match, before flagging
   the phrase. Literal-trie matches gain the same guard check.
3. Storage in `EntryData` (jsonb); UI + docs grow context-condition fields.

**Notes.** Guards are token-window predicates — cheap, and they need no offsets:
they run during `Check`, which already has the whole text. **Known limitation:**
because results are keyed by text, a phrase that appears in *both* a valid and
an invalid context within the *same input chunk* is still flagged for all its
occurrences in that chunk (the client underlines each; the user simply declines
the suggestion on the valid one). In practice this is rare and tolerable: each
input string is already a small UI-scoped chunk (a paragraph or headline), so a
guard only loses precision when one short chunk uses the same phrase both ways.
Guards therefore act at chunk granularity. Per-occurrence precision would
require returning positions; defer that unless the imprecision proves annoying
in practice. This is mostly *data modelling + a predicate check*, not new
matching machinery, provided item 4 landed first.

---

## 6. Mistakes with words in the middle (språkfel med ord emellan)

> "Vi kan inte fånga upp när språkfelet har ord i mitten… exempelvis 'Han kan
> inte längre varken se eller höra henne.'"

**Problem.** `PhraseIterator` only builds **contiguous** windows of ≤3 tokens.
A mistake pattern like "inte … varken" with arbitrary words between cannot be
expressed.

**Plan.**

1. Extend the rule pattern language (item 4) with a **gap/wildcard** token: a
   bounded skip (e.g. "0–N intervening words"). The Swedish example becomes a
   rule like `inte` `<gap 1–4 words>` `varken`.
2. Implement gap matching in the engine: anchor on the first literal token,
   then attempt to satisfy subsequent tokens within the allowed gap span. The
   matched span runs contiguously from the first to the last matched token (gap
   words included), so it becomes the entry's `Text` and is underlined by string
   like any other phrase; the suggestion drops/rewrites tokens via the template
   (`inte längre varken` → `inte längre`).
3. Bound gap width (configurable, small default) to keep matching linear and
   avoid pathological scans. `log` / surface when a configured bound truncates a
   would-be match so silent misses are visible.
4. Storage, UI, docs as for item 4.

**Notes / risks.** This is where the fixed 3-word window in `PhraseIterator`
must be replaced (or supplemented) by the token-stream-based engine. Gap rules
are inherently fuzzier — pair with item 5's guards and require explicit opt-in
per rule to limit false positives.

---

## Addressed without new work: words with several senses (flera betydelser)

> "Hur hanterar vi ord med flera betydelser, exempelvis major som är både en
> titel och en tävling… om jag lägger in 'majors' som fel/förslag träffar det
> också titeln major i genitiv."

This was considered as a roadmap item but does not need one — it is solvable
with capabilities that **exist today**. An ambiguous mistake string like
`majors` (the incorrect plural of the tournament sense, but the valid genitive
of the title sense) should be authored at **`suggestion` level rather than
`error`**, with a **`description`** that tells the editor which form applies
when, e.g. *"Tävling: en major, flera major. Genitiv av titeln (majors) är
korrekt."* The editor then sees a soft prompt and decides, instead of a hard
flag on a legitimate use.

Once context guards (item 5) land, they can additionally suppress the common
false-positive contexts for the ambiguous string. True sense disambiguation
(POS / lemma tagging, or hunspell morphology via the `Stem` wrapper in
`hunspell/hunspell.go`) remains out of scope — a separate investigation if the
suggestion-level approach ever proves insufficient.

---

## Unifying architecture

Items 4–6 are not three features; they are three views of one missing
capability: **a token-based rule engine with context guards.** Building these as
one-off hacks would multiply complexity; building the foundation once makes 5
and 6 mostly configuration.

### Token stream

Replace the "concatenate a 3-word window into a string" approach with a single
**tokenization pass** per `Check()` call that yields the classified tokens in
order (the segmenter already classifies token types; token positions are used
internally for neighbour/guard checks but are not exposed on the wire). Both the
existing literal-trie fast path and the new rule engine consume this stream. The
trie
stays as the optimization for pure-literal entries (the common case); only rules
that need classes/gaps/guards go through the general matcher.

### Rule model

A unified entry/rule is a sequence of **token matchers** plus optional
**context guards**:

- token matchers: literal, alternation (today's `Expand`), token class
  (`digit`, `word`, …), and bounded gap (item 6);
- guards: `before` / `after` / `not_before` / `not_after` (item 5);
- a **case policy** (item 2);
- a **replacement template** (literal string, or a transform such as
  "swap `-` for `–`", item 4);
- the existing `level` (error/suggestion), `status`, and `description`.

This subsumes today's literal phrases, `{A|B}` expansion, forms, and common
mistakes as the simplest rule shapes.

### Generating suggestions

For literal entries the suggestion is a **static string** (the canonical entry
text, or the mapped `Forms` value) — fine, because the mistake is a fixed string
too. Rules match *variable* text (`12-15`, `103-110`, a gap of arbitrary words),
so their suggestions must be **derived from what the match captured**. The
general mechanism is **capture + template instantiation** (the same idea as
`regexp.Regexp.Expand`):

1. **Token matchers capture spans.** Class tokens (`digit`, `word`) and gap
   tokens capture the concrete text they matched; literals don't need to.
   Captures are positional or named.
2. **The rule carries a replacement template** referencing those captures, e.g.
   `{1}–{2}` (or named `{num1}–{num2}`). Use a placeholder syntax distinct from
   the `{A|B}` *expansion* syntax so the two don't collide.
3. **At match time** the engine fills the template with the captured substrings,
   producing a concrete suggestion string.

The three editorial rule shapes map cleanly:

| Rule | Captures | Template | Suggestion for `…12-15…` / `…inte längre varken…` |
|------|----------|----------|------|
| dash (item 4) | `12`, `15` | `{1}–{2}` | `12–15` |
| literal (today) | — | `fängelse` (constant) | `fängelse` |
| gap (item 6) | gap=`längre` | `inte {gap}` (drops `varken`) | `inte längre` |

The static-string case is just the **degenerate template** with no placeholders,
so the current literal/forms behaviour is the same machine with an empty capture
set — nothing is special-cased.

Two cross-cutting details:

- **Case policy is a post-step.** After the template yields a suggestion, apply
  the entry's case policy (item 2): a case-folded, sentence-initial match
  capitalizes its suggestion. Keeping this as a single post-processing step
  covers literal and rule matches alike.
- **Suggestions are produced on demand, not stored.** Replacement is manual: the
  client underlines the flagged span, the user right-clicks, and the client
  calls the `Suggestions` RPC with that span. The template is instantiated
  against the text passed to that call, so repeated phrases are a non-issue and
  no character offsets are needed. **This does mean the `Suggestions` RPC must
  run the rule engine too** — today it does a `mistakeTrie` lookup plus hunspell
  (`internal/spellcheck.go`); it must additionally match the input against rules
  and expand the template (e.g. `Suggestions("12-15")` → `12–15`).
- **Built-in vs. user-defined.** Shipped rules (e.g. the dash) may use a
  hardcoded Go transform; user-authored rules use the capture+template form,
  validated on write so a template only references capture groups that exist.

### Why offsets aren't needed

An earlier draft proposed adding character offsets to `MisspelledEntry`. They
turn out to be unnecessary, given the manual, right-click-driven flow (`Check`
flags spans → client underlines → user right-clicks → `Suggestions` re-derives
options from the passed span → user picks). Neither suggestion generation nor
replacement needs to know *where* in the document a match was, and repeated
phrases don't matter. The only thing offsets would buy is per-occurrence
flagging precision for context-sensitive rules (item 5) in chunks that
use a phrase both validly and invalidly — a tolerable, deferrable limitation
(see item 5's notes). Keep the wire contract text-keyed; the only
`MisspelledEntry` addition is the `status` field from item 1.

### Storage

`EntryData` (`postgres/entry.go`, backed by `data jsonb`) is the natural home
for case policy, rule patterns, and guards — no table migration needed, just
new optional fields plus matching proto fields on `CustomEntry` and UI/doc
support. Keep `entry.go` (the hand-maintained custom-type file) as the single
typed definition.

### Suggested delivery order

1. **Item 1** (status + moderation) — quality-desk workflow; makes `status`
   meaningful. Independent of the engine.
2. **Item 2** (capitalization) — standalone quick win; introduces the per-entry
   case-policy field.
3. **Item 3** (broader search) — independent low-priority UI tweak; slot in
   anytime.
4. **Item 4** (regex/dash rule) — lands the token stream + rule engine with a
   small, safe pattern vocabulary and a useful built-in rule. Extend the
   `Suggestions` RPC to run rules so right-click suggestions work for them.
5. **Item 5** (context guards) — adds guards on top of the engine.
6. **Item 6** (gaps) — adds bounded gap matching.

### Cross-cutting concerns

- **Performance:** index rules by an anchor token so `Check()` stays roughly
  linear in text length; never scan every rule against every position.
- **Safety:** curate the pattern vocabulary; if raw regex is ever exposed, use
  RE2 (`regexp`), compile once at load, and validate on write.
- **False positives:** gap rules and ambiguous strings are the riskiest; gate
  gaps behind explicit per-rule opt-in, and prefer `suggestion` over `error`
  with an explanatory `description` when a correction is only sometimes right.
- **Sync path:** new rule data flows through the existing eventlog →
  `applyEvent` → `AddPhrase` pipeline unchanged; `entryAsPhrase`
  (`internal/service_entry_updates.go`) and `AddPhrase` grow to compile rules,
  but the LISTEN/NOTIFY + FanOut machinery is untouched.
- **Backwards compatibility:** every existing entry is the simplest rule shape
  (literal, case-sensitive, no guards), so migration is a no-op — old entries
  keep working as the engine's degenerate case.
