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

Measured on a 24-core workstation, `benchtime 3x`, Go 1.27. `ListChildren` is a 500-row page.

| Benchmark | Fixture | Time per page |
|---|---|---|
| `BenchmarkListChildren` (in memory) | 10,000 entries | **3.9 ms** |
| `BenchmarkListChildren` (in memory) | 100,000 entries | **70 ms** |
| `BenchmarkListChildrenPostgreSQL` | 10,000 entries | **5.6 ms** |
| `BenchmarkListChildrenPostgreSQL` | 100,000 entries | **38 ms** |
| `BenchmarkResolvePath` (in memory) | 10,000 siblings, 6 segments | **2.4 µs** |
| `BenchmarkCreateFile` (in memory) | 10,000 entries | **6.7 µs** |

The PostgreSQL run needs `TEST_DATABASE_URL`; it seeds its rows in one statement, because
creating 100,000 of them through the store takes minutes and a benchmark whose fixture dominates
its measurement is measuring the fixture.

### What the numbers say

**Path resolution does not care about directory size** — 2.4 µs at 10k, 3.2 µs at 100k. That is
the expected shape: resolution is a chain of per-level lookups, and the cost is the depth.

**A listing page costs more as the directory grows, on both backends.**

| | 10k entries | 100k entries | growth for 10× the data |
|---|---|---|---|
| in memory | 3.9 ms | 70 ms | 18× |
| PostgreSQL | 5.6 ms | 38 ms | 6.8× |

The page is always 500 rows, so neither number should move at all. The in-memory backend is the
honest one about what its code does — it sorts every child before applying the limit — but
PostgreSQL's 6.8× says its query is not behaving like an indexed top-N either: an index on
`(parent_id, name_key)` should make a page cost the same in a directory of ten entries and ten
million. That is the next thing to fix, and it is worth more than any of the numbers above: a
sync client lists every folder it tracks, so this multiplies by the size of the tree.

Getting the PostgreSQL figure required the harness to exist first. The in-memory number alone
would have hidden it, because it looks like "the in-memory backend is slow" rather than "the
listing query sorts a whole directory to return one page".
## Not measured

- **Upload throughput on a large file.** Needs a storage volume and a client outside the process.
- **A concurrent sync walk** (p95 across hundreds of clients). Needs a load generator; the
  concurrency tests assert correctness, not latency, which is a different question.
- **Idle resident memory.** Needs a running deployment, not a unit benchmark.
- **PostgreSQL figures**, for the reason above.

When those exist, they belong on this page next to the numbers above, with the hardware and the
fixture stated the same way.
