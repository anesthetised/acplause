# Built-in tools

[Documentation](README.md) · [Project overview](../README.md)

These tools work within the session's [Plan/Build policy](usage.md#plan-and-build-modes).
For client-supplied external tools, see [MCP servers](mcp.md).

| Tool          | Kind      | Description                                       |
| ------------- | --------- | ------------------------------------------------- |
| `read_file`   | `read`    | text files up to 1 MiB; `offset`, `limit` lines   |
| `list_dir`    | `read`    | tree, 3 levels, 200 entries                       |
| `search_code` | `search`  | literal or regex search, optional case folding, 50 matching lines |
| `get_instructions` | `read` | source-labelled root-to-directory `AGENTS.md` rules |
| `write_file`  | `edit`    | create or overwrite, up to 5 MiB                  |
| `edit`        | `edit`    | replace one exact occurrence in a UTF-8 file up to 5 MiB |
| `edit_batch`  | `edit`    | 1–50 ordered replacements in one file, one approved diff |
| `bash`        | `execute` | `bash -c` in the working directory, output capped |
| `ask_user`    | `other`   | one to three text/choice, numeric or boolean questions, when supported |
| `update_plan` | `other`   | replace the current turn's execution plan          |

## Searching and directory traversal

`search_code` is literal and case-sensitive by default: `{"pattern":"a.b"}`
matches the dot literally. Set `ignore_case: true` for Unicode case-insensitive
matching, or use `{"pattern":"^(TODO|FIXME):","regex":true,"ignore_case":true}`
to match a Go regular expression on each line. Results keep file order and line
numbers, with at most 50 matching lines and a 300-byte UTF-8-safe text preview
per line. `path` and `file_glob` narrow the search; text files are limited to
1 MiB. These options are also available in Plan mode.

To continue a search, set `offset` (zero-based matching-line offset) and `limit`
(1–50, default 50). `context_lines` (0–5, default 0) adds surrounding lines;
overlapping windows within a file are merged. Matches use `path:line:text`,
while context uses `path-line-text`. Only selected matching lines count toward
the page; a skipped or later match may appear as context and be selected on
another page. Every line keeps its original number and the same preview bound.
For example, `{"pattern":"TODO","limit":10,"context_lines":2}` returns at most
ten matches plus context. When any pagination/context option is explicit,
the result ends with `... more matches; next offset: N` or `... no more matches`;
an empty page reports `No more matches` with its offset. Calls omitting all three
options keep the original output format. Each page traverses files afresh in
lexical order, so edits between calls can shift offsets; pagination is not a
filesystem snapshot. Explicit null, fractional and out-of-range values fail
the tool without running the search.

`search_code` and `list_dir` skip hidden descendants by default. An explicit
hidden starting directory such as `{"path":".github"}` is inspectable;
`include_hidden: true` also reveals hidden descendants and can expose hidden
configuration to the model. Every `.git` entry is excluded case-insensitively,
including worktree metadata files. Explicitly traversing a `.git` path fails
the tool. Generated names (`node_modules`, `target`, `__pycache__`) remain
excluded during traversal, and directory symlinks are not followed. This policy does not interpret
`.gitignore` and does not restrict direct `read_file` or `bash` access.

## File edits

`write_file`, `edit` and `edit_batch` report what they changed as a diff in the tool call,
with a `git_patch` unless the change is too large to diff.

`edit_batch` takes a required `path` and `edits` array of 1–50 objects with
explicit string `old_text` and `new_text`. It operates on one existing regular
UTF-8 file; input and final output must each fit within 5 MiB. Replacements
apply in array order to an in-memory candidate, and each nonempty `old_text`
must occur exactly once at its step, including overlapping occurrences. An
empty `new_text` deletes the match. For example:

```json
{"path":"main.go","edits":[{"old_text":"oldName","new_text":"newName"},{"old_text":"newName()","new_text":"newName(ctx)"}]}
```

The whole batch is validated before one diff is shown for one permission.
Preview retains the original bytes and the final candidate. Before writing,
the tool rereads the bounded file and compares its bytes with that original.
A changed, deleted or nonregular target fails without writing and requires a
new read, call and approval. Failed validation or a rejection/cancellation while
permission is pending never writes. The successful diff describes the retained candidate that was
approved. This is validation before writing, not crash-atomic persistence or a
lock against external changes racing after the final comparison.

## Permissions

`write_file`, `edit`, `edit_batch` and `bash` run only after the user allows them through
`session/request_permission`, with the diff or command shown in the request.
acplause offers "allow once" and "reject once"; anything else, including a
cancelled or unrecognised answer, is a rejection, which the model is told
about. Automatic approval is the client's setting. See
[ADR 0003](adr/0003-permissions.md).

## Bash terminals

Permitted `bash` commands stream combined stdout/stderr into an agent-owned,
noninteractive ACP terminal. The client gets the command, working directory,
a terminal reference on the tool call, byte chunks and a final replacement
snapshot with the process's exit code or signal. Rejected commands and failed
process starts create no terminal. Commands continue to use the existing
permissions, timeout and process-group cancellation policy.

Terminal output retains the first 50,000 bytes; excess bytes are counted and
reported by an omission marker in the final snapshot. Live updates publish at
most every 100 ms in chunks up to 16 KiB. Resume replays one bounded snapshot
per terminal, ordered with ongoing output. Restarts preserve saved output and
mark interrupted terminals ended with unknown exit status; recent unsaved
chunks may be lost. The model still receives the usual bounded textual tool
result. Terminals provide no stdin or PTY and do not manage detached jobs.
On Unix, cancellation, timeout and shell exit send `SIGKILL` to the command's
process group. Descendants that change their process group or session can
survive, as reproduced in the [Seatbelt prototype](testing/seatbelt-prototype.md).
See [ADR 0014](adr/0014-stream-agent-terminals.md). Actual T3 streaming,
cancellation and restart replay were verified in the
[runtime smoke record](testing/t3-first-class-smoke.md).

## File sandbox

File tools are confined to the session's `cwd` and `additionalDirectories`
with `os.Root`, which blocks escapes through `..` and symlinks. `bash` is not
sandboxed.

Read, write and edit operations accept regular files only; pipes and devices
are rejected. `write_file.content` and `edit.new_text` must be explicit
strings: an empty string is valid, but an omitted or null value is an error.

## Project instructions

`AGENTS.md` at the root of the session's `cwd` is appended to the system
prompt and reread before each normal model round and once before manual
`/compact`, on the session worker. One compaction keeps a stable instruction
snapshot across its summary requests. Missing or deleted files clear prior
root instructions; unreadable, nonregular, invalid UTF-8 or oversized files
(over 32 KiB) are omitted with a warning that does not include their contents.
Restarts reread live rules before the next model request.

Use `get_instructions` with a required directory `path`, such as
`{"path":"internal/agent"}`, to read scoped rules before editing there. It
returns source-labelled `AGENTS.md` files from the selected workspace root to
that directory, in root-to-directory order. Later sources take precedence only
in their subtree. Relative paths start at cwd; absolute paths select the first
configured containing root, with no combination of unrelated roots. Lookup
checks at most 32 directories, with 32 KiB per file and 128 KiB total text.
Missing files are normal; invalid targets, unreadable sources or exceeded bounds
fail the tool without a partial chain. Each lookup rereads live files; saved
tool results still replay their original content.

This read tool needs no permission in Plan or Build. Lookup is explicit: there
is no recursive discovery, parent lookup above a workspace root, `CLAUDE.md`
fallback or watcher. Project prose does not override enforced tool policies,
and explicit lookup cannot guarantee that arbitrary shell or MCP writes obey
scoped rules. See [ADR 0019](adr/0019-scoped-project-instructions.md).

## Parallel reads

`ACPLAUSE_READ_CONCURRENCY` defaults to `1` (serial); values `2..8` opt into
parallel consecutive built-in `read_file`, `list_dir`, `search_code` and
`get_instructions` calls. Every other tool is a strict serial barrier, including
writes, bash, MCP (even read-only), permissions, forms and plans. A group holds
one root selection through completion; resume changes the roots used by the
next group or serial call. Filesystem contents themselves are not snapshotted.

Progress patches can interleave; final cards and model results commit in original
call order, with no next model request until every call has a result. Cancellation
joins active reads and accounts for skipped siblings. At most N calls and their
uncommitted outcomes occupy the read window; a slow first call can leave slots
idle and delay later completion cards. Parallel disk work may be slower and use
more memory. See [ADR 0021](adr/0021-parallel-builtin-reads.md) and the
[local benchmark record](testing/parallel-reads-benchmark.md).
