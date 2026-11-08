# ADR 0031: Calibrate the context budget from provider usage

- Status: proposed
- Date: 2026-11-09
- Issues: [#182](https://github.com/anesthetised/acplause/issues/182),
  [#183](https://github.com/anesthetised/acplause/issues/183)
- Supersedes: [ADR 0006](0006-context-compaction.md) decision 1 (size
  estimate and `max_tokens`); the rest of ADR 0006 and ADR 0009's image
  reserve stand
- Source: [OpenRouter parameters](https://openrouter.ai/docs/api/reference/parameters),
  [OpenRouter models](https://openrouter.ai/docs/api/api-reference/models/list-all-models-and-their-properties)

## Context

ADR 0006 counts one serialized byte as one token. Code and JSON run at
roughly 3–4 bytes per token, so trimming and compaction start at a quarter to
a third of the real window: a 200k model behaves like a 50k one, and every
unnecessary compaction costs a request, latency and summary fidelity. Every
completion already reports `usage.prompt_tokens`, which was only forwarded
as `usage_update`.

The same decision capped `max_tokens` at 8192. One response, reasoning
included, then cannot carry a `write_file` of more than about 30 KB: the
arguments are truncated, the call fails and the turn stops with `max_tokens`.

A request over the window has no recovery: the provider rejects it and the
turn fails. Any estimate must therefore err towards compacting early.

## Decisions

1. **Calibrate from usage.** After each completion that reports
   `prompt_tokens`, record the serialized bytes (with ADR 0006 framing) of the
   request that was sent divided by its prompt tokens. Ratios are kept per
   model for the whole process, shared by all its sessions and never
   persisted. Only the smallest observed ratio is kept, as the most
   conservative one. Summary requests calibrate like answer requests. A zero
   or missing count is ignored, and so is a ratio above 6 bytes per token:
   code, JSON and prose run at about 3–4.5, so a higher ratio means the
   provider undercounted (for example only uncached tokens).
2. **Images do not calibrate.** Requests that carry images are not used,
   since their size is ADR 0009's fixed reserve rather than bytes. That
   reserve, 8192 tokens per image plus 256 per message with images, is added
   unscaled to every estimate.
3. **Estimate with a margin, never above a byte per token.** A calibrated
   estimate is text bytes divided by the ratio, plus 20%, rounded up, but no
   more than the text bytes: one byte per token remains the conservative
   bound of ADR 0006, so calibration only ever relaxes it. Before the model
   has an observation, one byte still counts as one token. The input
   budget remains 75% of the advertised window, and an unknown window still
   disables the size check.
4. **Size the output by the window and the provider.** Send
   `max_tokens = window/4`, capped by the catalog's nonzero
   `top_provider.max_completion_tokens`. When that ceiling is unknown, or the
   window is unknown, omit `max_tokens` and leave the provider default. There
   is no fixed constant. Summary requests use the same value.

## Consequences

- Once one response has been counted, compaction starts near the real 75%
  threshold instead of at a quarter of it. The first request of each model in
  a process still uses the byte estimate.
- After the first observation, an unrepresentative response can only make
  estimates more conservative, because the smallest ratio wins, and never
  stricter than ADR 0006: an absurd overcount pins the model to one byte per
  token for the rest of the process, which is safe. An undercount within the
  plausible range (up to 6 bytes per token) makes estimates too small until a
  smaller ratio is seen; the margin is the only protection there, and an
  overflow still fails the turn.
- Restarting the process forgets calibration; storing it would tie session
  data to provider tokenizers for little gain.
- A large output reserve is part of the price check: OpenRouter reserves
  credit for the requested output and may answer 402 when the balance cannot
  cover it. Omitting `max_tokens` lets the provider apply its own default.
- Alternatives rejected: a fixed 3 bytes/token constant is still wrong per
  model and per content; a tokenizer dependency was rejected in ADR 0006 for
  portability; calibrating per session would restart from the byte estimate in
  every new conversation; keeping the latest ratio instead of the smallest
  could make estimates less conservative.
