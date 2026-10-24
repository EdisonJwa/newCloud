# Benchmark harness and baseline

The competitor comparison (§8 of `competitors.md`) says the number a buyer actually feels is
**files per second on a directory listing**, because "slow login and file listing" is the most
reported complaint against the systems this project is measured against. This page records the
harness for that number and a baseline measured with it.

It is a starting point, not a complete performance programme: two of the four measurements that
note asks for — sustained upload throughput and a concurrent sync walk — need infrastructure this
repository does not carry yet, and are listed as not measured at the bottom.

## How to run it

The benchmarks live in `internal/store/bench_test.go` and are not part of CI.

```sh
# Fixture of 10,000 entries (the default), 20 passes each.
go test ./internal/store -bench . -run '^$' -benchtime 20x

# The directory size the comparison calls for.
BENCH_CHILDREN=100000 go test ./internal/store -bench ListChildren -run '^$' -benchtime 3x
```

`BENCH_CHILDREN` sets how many entries the fixture directory holds, so one harness answers "10k"
and "100k" without editing code.

## What is measured

| Benchmark | Operation | Why this one |
|---|---|---|
| `BenchmarkListChildren` | One page (500 rows) of a directory listing | The listing a sync client performs on every folder it tracks. This is the §8 number. |
| `BenchmarkResolvePath` | Resolve a six-segment path | Every request resolves its path before it does anything, so this is paid on every call of every protocol. |
| `BenchmarkCreateFile` | Create one file in one transaction | The write path an upload or a sync push hits hardest. |

All three run against the **in-memory backend**, which is deliberate: it removes disk and network
from the measurement, so the numbers are the application's own cost. A PostgreSQL run would be the
production number and needs the fixture helpers in `pg_test.go`; that is not wired up yet.

## Baseline

Measured on a 24-core workstation, `benchtime 20x` (3x for the 100k fixture), Go 1.27.

| Benchmark | Fixture | Time per operation |
|---|---|---|
| `BenchmarkListChildren` | 10,000 entries in the directory | **3.5 ms** per 500-row page |
| `BenchmarkListChildren` | 100,000 entries | **66 ms** per 500-row page |
| `BenchmarkResolvePath` | 10,000 siblings, 6 segments | **2.4 µs** |
| `BenchmarkResolvePath` | 100,000 siblings, 6 segments | **3.2 µs** |
| `BenchmarkCreateFile` | 10,000 entries in the directory | **6.7 µs** |

### What the numbers say

**Path resolution does not care about directory size** — 2.4 µs at 10k, 3.2 µs at 100k. That is
the expected shape: resolution is a chain of per-level lookups, and the cost is the depth.

**Listing scales with the size of the directory, not with the size of the page.** A page is
always 500 rows; the time is not constant. 3.5 ms at 10,000 entries and 66 ms at 100,000 means a
directory ten times larger costs nineteen times more for the same page. That is what the in-memory
backend's implementation does — it sorts every child before applying the limit — and it is the
difference between "listing is fine" and the complaint §8 is about.

This is a finding about the in-memory backend, not a claim about a deployment: PostgreSQL orders
with an index and applies the limit in the database, so the production path should not share this
shape. Measuring that is the first thing this harness should be extended to do, because the
in-memory number is the one a test suite pays for and the PostgreSQL number is the one a user
pays for — and only one of them has been measured.

## Not measured

- **Upload throughput on a large file.** Needs a storage volume and a client outside the process.
- **A concurrent sync walk** (p95 across hundreds of clients). Needs a load generator; the
  concurrency tests assert correctness, not latency, which is a different question.
- **Idle resident memory.** Needs a running deployment, not a unit benchmark.
- **PostgreSQL figures**, for the reason above.

When those exist, they belong on this page next to the numbers above, with the hardware and the
fixture stated the same way.
