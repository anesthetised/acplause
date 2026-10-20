# ADR 0019: Resolve scoped project instructions explicitly

- Status: accepted; resolver and lookup/refresh activation implemented (#78/#79)
- Date: 2026-10-20
- Issues: [#78](https://github.com/anesthetised/acplause/issues/78), [#79](https://github.com/anesthetised/acplause/issues/79)
- Scope change: Extends the previous root-only, read-once instruction policy

## Context

The current agent reads only `cwd/AGENTS.md` when creating a session and appends
its trimmed text to the system message. Subtree rules remain undiscovered and
edits to root instructions remain invisible for that session. Recursively
injecting every instruction file would add unrelated rules and unbounded prompt
overhead. Establish a bounded resolver before changing agent requests.

## Decisions

1. Scoped lookup names an existing target directory and uses the same workspace
   root selection as other file tools. Relative paths retain cwd semantics;
   absolute paths select the first configured root containing them, rather than
   the most specific overlapping root. Do not walk above that selected root or
   combine rules from unrelated roots.
2. Read `AGENTS.md` along the requested lexical directory chain, including the
   selected root and target, in root-to-leaf order. Return each source's absolute
   path and exact validated UTF-8 text, preserving whitespace and empty files.
   Later sources take precedence only within their directory's subtree. The
   resolver returns prose and does not interpret or enforce it.
3. Inspect at most **32 directories**, counting the root and target, with at
   most **32 KiB per file** and **128 KiB aggregate text**. Bounds count bytes;
   exact limits are accepted. Exceeding any bound fails without truncation or a
   partial chain. Retain only the bounded chain and returned instruction text.
4. A missing instruction file is normal. A present unreadable, nonregular,
   dangling-symlink or invalid UTF-8 source fails with its source path. A missing
   or nondirectory target fails too. Use existing descriptor-checked bounded
   reads and cancellation; metadata alone cannot authorize a regular-file read.
   All access remains under `os.Root`: relative symlinks must stay within that
   root and absolute symlinks are rejected. Accepted aliases retain their
   requested lexical labels and ancestor chain.
5. Reread sources on every lookup. Add no persistent cache, directory watcher,
   recursive discovery or network source. Concurrent filesystem changes do not
   form an atomic snapshot. Return no partial chain on cancellation or errors,
   and never log instruction contents.
6. #79 exposes `get_instructions` with a required directory
   `path`, source labels and a precedence explanation. Allow this built-in read
   tool in Plan and Build without permission. Teach the model to look up the
   containing directory before editing there. Explicit lookup cannot guarantee
   that arbitrary shell or MCP writes honor prose; enforced mode, sandbox and
   permission policy always take precedence.
7. Replace the previous read-once behavior with a root refresh
   before each normal model round and once before manual compaction. Perform
   I/O on the worker, outside session locks and the JSON-RPC read loop. Publish
   only against the retained toolbox/root generation that was read; retry or
   discard stale reads after resume. Missing/deleted files clear old root text;
   root read errors retain the existing warn-and-omit behavior, without stale
   fallback. Preserve system-message mode, directories and compaction accounting.
   One compaction uses a stable snapshot, without refresh per summary chunk.
8. #78 supplied the bounded resolver; #79 activates lookup/refresh, keeping the
   existing root `Instructions` reader's trim and warn-and-omit semantics.
   No `CLAUDE.md` fallback, parent lookup
   above a selected workspace root, persistence migration or ACP schema change.

## Consequences

Source labels keep subtree rules distinguishable and bounded lookup makes
instruction costs predictable. Callers must explicitly request nested rules;
unrelated siblings are never searched. Fresh reads avoid cache invalidation,
while failures expose unusable scoped instructions instead of silently dropping
rules. Root refresh retains its separate compatibility policy. Existing tool
permissions and Plan restrictions remain enforced independently of project text.
