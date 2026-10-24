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

Measured on a 24-core workstation, Go 1.27. `ListChildren` is a 500-row page.

| Benchmark | Fixture | Time per page |
|---|---|---|
| `BenchmarkListChildren` (in memory) | 10,000 entries | **3.9 ms** |
| `BenchmarkListChildren` (in memory) | 100,000 entries | **70 ms** |
| `BenchmarkListChildrenPostgreSQL` | 10,000 entries | **0.92 ms** |
| `BenchmarkListChildrenPostgreSQL` | 100,000 entries | **1.59 ms** |
| `BenchmarkResolvePath` (in memory) | 10,000 siblings, 6 segments | **2.4 µs** |
| `BenchmarkCreateFile` (in memory) | 10,000 entries | **6.7 µs** |

The PostgreSQL run needs `TEST_DATABASE_URL`; it seeds its rows in one statement and then runs
`ANALYZE`, because creating 100,000 of them through the store takes minutes, and because without
statistics the planner picks a plan no deployment would run.

### What the numbers say

**A listing page is flat on PostgreSQL now, and it was not before.** The page is the same 500 rows
whatever the directory size, and the PostgreSQL figure grew 6.8× for ten times the data (5.6 ms at
10k, 38 ms at 100k) until `migrations/0018_listing_index.sql` added an index on
`(parent_id, name) WHERE trashed_at IS NULL` — the order the query sorts by. With it: 0.92 ms and
1.59 ms, 6× and 24× faster, and a growth of 1.7× for 10× the data instead of 6.8×.

**The first measurement of the fix was wrong, and the reason is worth keeping.** After adding the
index the 100k page still measured 30 ms, which looked like the index had not helped. The plan was
fine — an index scan of 500 rows taking 0.11 ms — but the benchmark's own fixture had bulk-loaded
100,000 rows and never run `ANALYZE`, so the planner estimated badly and sorted the directory. The
fixture now analyzes; a benchmark that measures a plan no deployment would ever choose is measuring
itself.

**Path resolution does not care about directory size** — 2.4 µs at 10k, 3.2 µs at 100k. That is the
expected shape: resolution is a chain of per-level lookups, and the cost is the depth.

**The in-memory backend still sorts the whole child set** — 3.9 ms at 10k and 70 ms at 100k, 18×
for ten times the data. It is a test/dev backend, not a deployment, and it is the one number here
that is still shaped wrong; matching PostgreSQL would mean an ordered index in memory, which is
worth doing only if the memory backend stops being test-only.
## Not measured

- **Upload throughput on a large file.** Needs a storage volume and a client outside the process.
- **A concurrent sync walk** (p95 across hundreds of clients). Needs a load generator; the
  concurrency tests assert correctness, not latency, which is a different question.
- **Idle resident memory.** Needs a running deployment, not a unit benchmark.
- **PostgreSQL figures**, for the reason above.

When those exist, they belong on this page next to the numbers above, with the hardware and the
fixture stated the same way.
