# ADR 0005: Remember the last selected model and reasoning effort

- Status: accepted
- Date: 2026-10-19
- Issue: #10
- Supersedes: ADR 0002 decision 2 for new sessions with an available remembered model

## Context

Starting every session with "Select a model" makes the user repeat a choice
they already made. SQLite now provides a shared place to remember it across
restarts and editor windows. Reusing that explicit choice preserves the
intent of ADR 0002: the agent must never pick an arbitrary model.

## Decisions

1. **Remember explicit selections.** Successful `session/set_config_option`
   changes to model or effort save the pair in a single `settings` row in
   the session database. The preference belongs to the data directory,
   across workspaces and processes. No model catalog is stored.
2. **Reuse an available selection.** A new session starts with the remembered
   model only if it is in the current tool-capable catalog. A valid remembered
   effort is reused when that model supports reasoning; otherwise effort
   stays at the provider's default.
3. **No substitute model.** Without a remembered model, or when it is absent
   from the catalog, show "Select a model" and reject prompts until the user
   selects one, as before. Keep the remembered choice so a temporarily
   unavailable model can be reused when it returns. Catalog loading failures
   still fail session creation.
4. **Only user changes update the preference.** Creating, resuming, saving,
   closing or deleting a session does not change it. Resumed sessions retain
   their own model and effort; invalid config requests save nothing.
5. **Keep the read loop responsive.** Preference writes run in the background
   in config request order within a process. New sessions wait for earlier
   preference writes, and orderly shutdown drains them. Each new session
   reads SQLite again to see choices saved by other processes; across
   processes, the last committed pair wins.
6. **Storage failures.** Log preference read/write failures to stderr. A
   failed read leaves the new session without a selected model; a failed
   write can lose the remembered choice. Session config changes still
   succeed in memory, following ADR 0004's storage failure policy.

## Consequences

- A new session normally needs no model selection before its first prompt.
- The first selection is still explicit, and no fallback model is chosen.
- A forward migration adds settings without deriving a preference from old
  sessions, whose last save need not be the user's last selection.
- Config responses acknowledge the in-memory change; a crash before the
  background write finishes can lose the remembered preference.
