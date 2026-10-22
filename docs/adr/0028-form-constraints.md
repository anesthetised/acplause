# ADR 0028: Add bounded constraints, defaults and titled choices to structured questions

- Status: accepted
- Date: 2026-10-22
- Issue: [#138](https://github.com/anesthetised/acplause/issues/138); `pattern` follow-up [#159](https://github.com/anesthetised/acplause/issues/159)
- Scope change: Extends the question fields of ADRs 0011, 0017 and 0027
- Source: ACP `2.0.0-alpha.9` at `3be9cfc` (`testdata/schema/v2/VERSION`) and
  its `docs/protocol/v2/elicitation.mdx`

## Context

`ask_user` can request typed scalars and string choices (ADR 0027), but every
field is required, unconstrained and untitled. The pinned schema also defines
form and field titles/descriptions, string length/pattern/format, numeric
bounds, defaults, titled single-select (`oneOf`) and multiselect
(`items.anyOf`) options, and `minItems`/`maxItems`.

What the pinned sources say, and do not say:

- Every optional schema field treats omitted and `null` as equivalent.
- Response content values may not be `null`; `content` itself may be omitted
  or `null` on `accept`.
- Defaults are a presentation aid: clients "SHOULD pre-populate" them.
  Clients and agents "SHOULD" both validate submitted values.
- `pattern` is only marked `format: regex`; the docs define no dialect.
  Go's RE2 and ECMA-262 differ, so a client may accept what Go rejects and
  the reverse.
- String length has no stated unit. JSON Schema counts code points.
- `date` is `YYYY-MM-DD`, `date-time` is RFC 3339; `email` and `uri` are
  unspecified. Unknown formats are annotations, never errors.

Not covered here: URL and request-scoped elicitation (#139/#140), auth
(ADR 0023), parked T3 compatibility work.

## Decision

The maintainer accepted this design on 2026-10-22, with `pattern` deferred.

1. **Model-facing shape.** Extend each question with optional fields; no
   arbitrary JSON Schema from the model. Add optional top-level `title` and
   `description` mapped to the schema's. Constraint fields are bound to the
   type: misuse (for example `minLength` on an integer) fails the call.
   Unknown unrelated fields remain ignored.
2. **Required and optional.** New `required` boolean, default `true`, so
   existing calls are unchanged. The wire `required` list names only required
   fields.
3. **Answers.** An accepted answer may omit optional keys; unknown keys, `null`
   values and wrong types fail. Required keys must be present. Accepted
   content that is omitted or `null` is an empty object. The result contains
   only answered fields: an omitted optional is *unanswered*, never filled from
   a default and never `null`. Boolean `false`, `0` and `""` stay real answers.
4. **Defaults.** Optional `default` per type. It must satisfy the field's own
   type, bounds, pattern/format and choices, or the call fails before any form
   is sent. Defaults never become answers and never authorize anything. Zero
   values (`0`, `false`, `""`, empty array) are sent, so wire fields use
   presence-aware types.
5. **String limits.** `minLength`/`maxLength` count Unicode code points, are
   non-negative, `min <= max`, and fit uint32. Answers are validated in the
   same unit.
6. **Numeric bounds.** `minimum`/`maximum` are inclusive. Integer bounds are
   int64 tokens; number bounds are finite float64. `minimum <= maximum`.
   Answers are checked against them after the type check from ADR 0027.
7. **Formats.** Accept any short `format` string and pass it through.
   Validate answers for `date` (`time.DateOnly`) and `date-time`
   (`time.RFC3339`) with the standard library. Validate `email` (a bare
   address: `net/mail` parse with identical `Address`, no display name) and
   `uri` (`net/url` parse with a scheme). Unknown formats are annotations and
   are not validated.
8. **Pattern.** Deferred. The pinned field is resolved as not supported: the
   tool rejects `pattern` and sends none. Tracked in
   [#159](https://github.com/anesthetised/acplause/issues/159); a dialect
   decision needs a new ADR.
9. **Titled choices.** New `choices` array of
   `{value, title, description?}` alongside the existing `options` strings;
   exactly one of the two may be set on a question. `choices` project to
   `oneOf` (`const`/`title`/`description`) for single-select and to
   `items.anyOf` for multiselect; `options` still project to `enum`. Values
   are the machine identity: unique, bounded like options today; titles are
   labels only and are never accepted as answers. Same limits as today:
   at most ten, `value` <= 200, `title` <= 120, `description` <= 500
   characters.
10. **Multiselect counts.** Optional `minItems`/`maxItems` with
    `0 <= minItems <= maxItems <= len(choices)`; today's implicit
    `maxItems = len(options)` stays the default. Answer arrays must have unique
    offered values within the counts. `default` is a unique subset of the
    offered values within the counts.
11. **Unchanged.** `mode: form`, flattened scope, form capability negotiation,
    cancellable requests, `requires_action`/`running` transitions, failed
    results for invalid answers, interrupted-question recovery and late-answer
    handling. Persistence stores tool arguments and results as text exactly as
    before; no migration. No new dependency (stdlib only).

## Alternatives considered

### Pattern

- **Defer (accepted).** Resolves the pinned field as "not supported" and
  records why. No client/agent disagreement, no new code. Cost: models cannot
  request regex-constrained text; they can still use formats, lengths, or
  validate after the answer.
- **RE2/ECMA-262 common subset.** Accept only literals, classes, anchors and
  simple quantifiers, whose meaning is identical in both dialects, and
  validate with Go. Gives real support, but needs a small parser to enforce
  the subset and tests for its edges; subtle differences (for example
  `\d`, `\w`, `$` and case handling on Unicode) are easy to get wrong.
- **Pass through, validate in Go.** Rejected: silently substitutes RE2
  semantics for an unstated dialect, which the issue forbids.
- **Pass through, trust the client.** Rejected: the agent must validate again.

### Titled choices

- **Separate `choices` field (accepted).** Keeps `options` a plain string
  array, which is the easiest shape for OpenAI-compatible providers to follow
  and keeps old calls byte-identical.
- **Polymorphic `options` (string or object).** One field, but `anyOf` items
  in the tool's parameter schema are handled unevenly across providers, and
  mixed arrays need an extra rule.

### Formats

- **Validate the four known formats (accepted).** Matches the
  "agents SHOULD validate" rule; the email/uri rules above are deliberately
  strict and documented.
- **Pass all formats through unvalidated.** Smaller, but a malformed date can
  reach the model as a "validated" answer.

## Verification and activation

Unit tests: every constraint accept/reject at and around its boundary,
conflicting or inconsistent constraints (`min > max`, defaults outside bounds
or not in choices, duplicate values, empty choice sets, titled/untitled mix,
misplaced fields), Unicode length (multi-byte and combining sequences), each
known format with good and bad samples and an unknown format as annotation,
optional keys omitted, `null` content and `null` values, empty multiselect
arrays, zero/false defaults, and old calls unchanged.

In-package handler tests and schema-validating e2e: a mixed form with titled
single and multiselect choices, optional omitted answers, defaults on the wire
without being injected into results, invalid answers, cancel and late answers,
and restart/resume of completed answers.

On implementation, update the tool description, `docs/usage.md`,
`docs/tools.md` and ADRs 0011/0017/0027 notes, then run focused tests,
`just check` and measured total Go coverage >= 80%. This decision does not
activate URL or request-scoped elicitation or resume the parked T3 work.
