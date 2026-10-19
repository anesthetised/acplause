# ADR 0018: Group model choices by their ID namespace

- Status: accepted
- Date: 2026-10-20
- Scope change: Extends ADR 0002's model selector presentation
- Source: ACP `2.0.0-alpha.9` at `3be9cfc` (`testdata/schema/v2/VERSION`)

## Context

OpenRouter catalogs can contain many model families. ACP select options allow
either a flat list or labeled groups without another capability negotiation.
Generic OpenAI-compatible backends may expose IDs without any provider prefix.

## Decisions

1. Group models by the nonempty prefix before the first slash, such as `openai`
   in `openai/model`. This is an ID namespace, not discovery of the actual serving
   provider. Keep IDs and display names unchanged.
2. Use stable `provider:<prefix>` group IDs and the prefix as the label. IDs
   without both a nonempty prefix and suffix belong to `unqualified`, labeled
   "Other models". Sort groups by ID and choices within groups by model ID.
3. Use grouped output only when at least two groups exist after the tool-support
   filter. Single-group catalogs retain their flat list. Do not mix flat values
   and groups in one options list.
4. With no selected model, insert the existing empty-ID "Select a model" value
   first: inside a separate `selection` group for grouped lists, or directly in
   flat lists. It remains unselectable and disappears after selection. Remembered
   selections and request validation are unchanged.
5. Return the same layout in new/resume/set_config_option responses. Mode and
   reasoning effort remain flat. No provider lookup, mapping or dependency is
   introduced. No T3-specific fallback or capability is invented.

## Consequences

Clients supporting standard groups can display provider headers. Protocol tests
validate both forms; actual T3 rendering remains unverified and its compatibility
issues remain parked.
