# Parallel built-in reads: local benchmark record

Recorded 2026-10-20 for #85, after the outcome/commit separation in #84.
Default concurrency remains 1; these measurements do not establish real-T3
acceptance or predict performance on other filesystems.

## Environment and method

- Go 1.27.2, darwin/arm64; macOS 27.0.1; Apple M1 Max.
- Benchmark suffix `-10`: 10 logical CPUs available to Go.
- Command: `go test ./internal/agent -run '^$' -bench ParallelRead -benchmem`.
- One operation runs eight calls through the actual group executor, tool
  preparation, sandboxed file access, ACP patch merging and ordered commits.
- `read_file`: eight different files. `search_code`: eight searches for an
  absent marker across the same 16-file directory. Each fixture file contains
  2,048 repeated text lines. Temporary local files, warm filesystem cache;
  no deliberate cache eviction or simulated I/O delay.
- Notifications are discarded and persistence is disabled to isolate local
  execution/commit overhead. No provider, network, shell, MCP or permission
  work is included. Full protocol/storage behavior has separate tests.
- One local benchmark run; no timing-ratio assertion in CI.

## Measurements

| Calls per operation | Workers | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| 8 × `read_file` | 1 | 1,046,643 | 2,096,687 | 539 |
| 8 × `read_file` | 2 | 731,149 | 2,204,270 | 576 |
| 8 × `read_file` | 4 | 594,283 | 2,256,772 | 585 |
| 8 × `search_code` | 1 | 13,205,495 | 17,393,696 | 5,192 |
| 8 × `search_code` | 2 | 8,490,401 | 17,392,297 | 5,218 |
| 8 × `search_code` | 4 | 6,901,338 | 17,391,067 | 5,225 |

This fixture was faster at 2/4 workers, with some extra read allocations.
Cold disks, resource contention and slower prefix calls can reverse that result.
The executor bounds both active work and buffered outcomes to one N-call
window; it refills slots as the ordered prefix commits. This keeps result memory
bounded but may leave slots idle behind a slow earlier read.
