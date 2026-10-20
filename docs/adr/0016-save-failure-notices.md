# ADR 0016: Report save failures through live ACP notices

- Status: accepted
- Date: 2026-10-20
- Scope change: Extends ADR 0004's in-memory fallback with user-visible guidance
- Source: ACP `2.0.0-alpha.9` at `3be9cfc` (`testdata/schema/v2/VERSION`)

## Context

Save failures keep work running in memory, but stderr alone does not tell an
editor user that recent changes may be lost on exit. ACP defines capability-free
`notice` updates as live information that clients may ignore.

## Decisions

1. Send one warning notice per uninterrupted save-failure episode, including a
   failed prompt insertion save that falls back to memory. A successful session
   save resets suppression. Keep retrying at existing save boundaries.
2. Use fixed plain-text guidance. Internal errors stay in stderr; database paths,
   queries and error text are not copied into notices.
3. Send under the session lock, through the usual connection writer. Do not store
   notices in the transcript, replay them, or add them to model history.
4. Ownership loss and cancellation retain their existing handling. Closing or
   lost sessions do not emit save warnings. Notices never authorize an operation
   or substitute for required prompt responses and lifecycle updates.

## Consequences

Clients that display notices can explain the durability risk. Clients that ignore
them retain the existing behavior. No database migration or capability is needed.
T3 rendering is unverified; its compatibility issues remain parked.
