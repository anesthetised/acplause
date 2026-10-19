# ADR 0002: Let the ACP client select the model

- Status: accepted; decision 2 partially superseded by [ADR 0005](0005-remember-model-selection.md)
- Date: 2026-10-18

## Context

Requiring a model in the environment duplicates the ACP client's model
selector. Choosing an arbitrary default could send a prompt to a model
the user did not intend to use.

## Decisions

1. Remove the model environment setting. Load selectable models from the
   provider's `/models` endpoint, retaining the existing tool support filter.
2. Start each session without a selected model. ACP requires `currentValue`,
   so the model selector initially includes an empty id named "Select a
   model". This placeholder is not a model and cannot be selected through
   `session/set_config_option`; it disappears after a real model is selected.
3. Reject prompts without a selected model with `-32602`, before inserting
   a message or contacting the provider.
4. Fail `session/new` if the catalog cannot be loaded or has no usable
   models. Preserve the authentication-required error for authentication
   failures.

## Consequences

- Clients must select a model before the first prompt in each new session
  unless an available remembered choice is reused (ADR 0005).
- Model selection is preserved when resuming a session.
- Providers must expose a usable `/models` endpoint.
- Selecting the first available model automatically was rejected because
  it could cause unintended model use and cost.
