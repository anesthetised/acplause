# ADR 0027: Add numeric and boolean fields to structured questions

- Status: accepted
- Date: 2026-10-22
- Issue: [#137](https://github.com/anesthetised/acplause/issues/137)
- Scope change: Extends the field types in ADRs 0011 and 0017
- Source: ACP `2.0.0-alpha.9` at `3be9cfc` (`testdata/schema/v2/VERSION`)

## Context

The pinned ACP form schema supports string, integer, number, boolean and
string-array answers. The existing `ask_user` tool exposes strings and
string choices, including multiselect, but cannot request a typed count,
numeric value or boolean. Asking for those as text leaves validation and
interpretation to the model.

This proposal covers the additional scalar types only. Form constraints,
defaults, optional fields and titled choices belong to #138. URL and
request-scoped elicitation belong to #139/#140. Auth stays excluded by
ADR 0023. Actual T3 compatibility issues remain parked.

## Decision

The maintainer accepted explicit optional types and strict answer validation
on 2026-10-22.

1. Add optional `type` to each existing question. Accepted values are
   `string`, `integer`, `number` and `boolean`. Omission means `string`,
   retaining existing calls without a new tool or API version. Explicit
   null, an empty type or an unsupported type fails the tool call; unknown
   unrelated fields remain ignored as before.
2. Keep the existing message/question fields and bounds: one to three
   required questions, the same ID/title/text lengths and option limits.
   `options` and `multiSelect: true` apply only to string questions. An
   explicitly typed string may still use the existing multiselect flag,
   which projects to the existing ACP array schema. Nonempty choices or
   enabled multiselect on another scalar type fail before sending a form.
3. Project the requested scalar type directly into the pinned
   `ElicitationPropertySchema`. The request retains `mode: form`, flattened
   `sessionId`/`toolCallId` and the existing required-key list. No new
   capability or arbitrary model-supplied JSON Schema is needed.
4. Accept only an explicit `action: accept` with exactly the requested keys
   and non-null values of the requested types. Boolean answers must be JSON
   booleans, including `false`. Integer answers must be integer JSON tokens
   within signed 64-bit bounds; decimal/exponent representations are not
   coerced into integers. Number answers must be JSON numbers representable
   as finite float64 values; integer tokens also satisfy a number field.
   Numeric strings and boolean strings fail validation. Zero, negative and
   fractional numbers remain real answers rather than missing values.
5. Preserve validated numeric tokens in the tool result so int64 boundary
   values and numeric text do not pass through a lossy float64 round trip.
   Retain the existing string and array normalization. Results are JSON
   objects in tool-result text/rawOutput as today, with numeric and boolean
   values rather than quoted strings. The same content survives history
   persistence and replay; no database migration is needed.
6. Keep form negotiation, pending/requires_action/running transitions,
   cancellable requests, failed results for declined/invalid responses,
   interrupted-question recovery and late-answer handling unchanged.
   Boolean answers request information and never grant tool permission.
   Only existing stdlib JSON parsing is required; add no dependency.

## Alternatives considered

- **Explicit optional types (recommended):** adds typed validation while
  preserving existing callers and keeping the model-facing tool bounded.
- **Continue asking for strings:** needs no implementation but cannot
  guarantee typed answers or separate invalid input from valid values.
- **Expose arbitrary form schemas:** would unlock more fields at once but
  expands schema validation and model error handling beyond this slice.
  The remaining bounded form features have a separate issue, #138.

## Verification and activation

Add argument and answer tests for every new type, incompatible choices,
false/zero, signed int64 boundaries, overflow, finite-number range,
fractions, exponent forms, numeric strings, null and omitted answers.
Retain existing unknown-field, string/enum and multiselect tests. Explicitly
test the chosen integer-token policy rather than relying on decoder details.

Add in-package handler coverage and schema-validating e2e for a mixed
integer/number/boolean form, typed tool-result delivery, invalid responses,
cancel/late-answer handling and restart/resume of completed typed answers.
Existing interrupted-question tests must still prove no form is reissued.
Verify no permission request is satisfied by accepting a boolean answer.

On implementation, update the question-tool description and user guides,
then run focused tests, `just check` and measured total Go coverage >=80%.
This decision does not activate the remaining form features in #138 or
resume the parked T3 acceptance work.
