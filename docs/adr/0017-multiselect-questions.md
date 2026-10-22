# ADR 0017: Allow multiple choices in structured questions

- Status: accepted
- Date: 2026-10-20
- Scope change: Extends ADR 0011's string-only form surface
- Source: ACP `2.0.0-alpha.9` at `3be9cfc` (`testdata/schema/v2/VERSION`)

## Context

Some coding questions need several choices, such as which components or checks to
include. ACP form elicitation already supports arrays of enum strings without a
separate capability beyond form support.

## Decisions

1. Add optional `multiSelect: true` to each `ask_user` question. It requires one
   to ten existing bounded, unique options. Omission or false retains the current
   string/single-choice contract. Do not accept arbitrary model-provided schemas.
2. Emit a required `type: "array"` field with `items: {type: "string", enum: [...]}`
   and `maxItems` equal to the offered count. An explicit empty array means none;
   missing and null answers are not empty selections.
3. Validate every accepted array: only offered strings, no duplicates. Preserve
   selection order and array types in the tool result and history. Existing
   validation of actions, exact keys, string fields and permissions remains.
4. Keep existing cancellation, connection-local forms and recovery behavior.
   Numeric/boolean form fields were outside this slice and are subsequently
   added by [ADR 0027](0027-typed-user-questions.md). Defaults and URL mode
   remain outside these decisions.

## Consequences

Models can ask for several choices using the existing `ask_user` tool. Protocol
tests validate emitted forms against the pinned schema; client rendering is not
assumed, and T3 compatibility work remains parked.
