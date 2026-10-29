# cheesedb benchmarks

Each number is the **median of 3 runs** (`-count=3`). Raw output was produced by the commands in "How to reproduce".

**Honesty rule:** the README may only claim an advantage shown here, and this file reports where cheesedb loses too.

## Machine

| | |
|---|---|
| CPU | Apple M1 Max, 10 cores |
| RAM | 64 GB |
| Disk | internal SSD, APFS |
| OS | macOS 26.5.2 |
| Go | 1.26.5 (darwin/arm64) |
| SQLite | 3.53.4, through `github.com/mattn/go-sqlite3` v1.14.52 (the C library, via cgo) |
| PostgreSQL | 17.11 (Homebrew), over TCP on 127.0.0.1, through `github.com/jackc/pgx/v5` v5.11.0 |

## What is compared

All three engines run the **same workload** (`internal/benchgen`), built from a fixed seed:
- every 10th record is a lot;
- each memory has 1-2 legs of 1-2 parents, chosen from the previous 1,000 records;
- every lot and memory holds 100 bytes of content.

Sizes: 10k, 100k and 1M memories (plus about 11% more records as lots).

**Same durability everywhere.** A write is acknowledged only after it is flushed to the physical disk with `F_FULLFSYNC`, the strongest flush on macOS:
- cheesedb: Go's `File.Sync` uses `F_FULLFSYNC` on macOS;
- SQLite: WAL mode, `synchronous=FULL`, `fullfsync=ON`;
- PostgreSQL: `fsync=on`, `synchronous_commit=on`, `wal_sync_method=fsync_writethrough`.

**How the SQL versions work.** They store the ledger the way a developer would on a general database:
- tables `nodes`, `legs` and `leg_parents` (indexed by parent), plus `recalls`;
- the same counter algorithm cheesedb uses (`dead_parents`, `alive_legs`), run as SQL statements in one transaction per operation;
- one client connection.

They are a straightforward port, not hand-tuned SQL. See "Caveats".

## Results

### Speed per operation

| Operation | Size | cheesedb | SQLite | PostgreSQL |
|---|---|---|---|---|
| **Remember** (one memory, durable) | 10k | 4.87 ms | 5.33 ms | 6.40 ms |
| | 100k | 4.74 ms | 5.32 ms | 6.19 ms |
| | 1M | 4.80 ms | 5.21 ms | 6.29 ms |
| **Get** (one memory by id) | 10k | 1.27 µs | 5.33 µs | 40.6 µs |
| | 100k | 1.45 µs | 6.64 µs | 40.7 µs |
| | 1M | 1.50 µs | 7.12 µs | 43.9 µs |
| **Recall**, removes 20 memories (durable) | 10k | 4.97 ms | 7.10 ms | 10.7 ms |
| | 100k | 4.99 ms | 7.01 ms | 10.6 ms |
| | 1M | 4.94 ms | 7.34 ms | 10.6 ms |
| **Recall**, removes 10,000 memories (durable) | 1M | ~5.05 ms (computed, see below) | 230 ms | 1.73 s |
| **Startup**: ready to answer the first query | 10k | 5.56 ms | 0.10 ms | 2.78 ms |
| | 100k | 55.1 ms | 0.10 ms | 2.80 ms |
| | 1M | **542 ms** | 0.10 ms | 2.78 ms |

### Startup with the hash chain (checked on every Open)

Replay recomputes every record's chain hash, so a tampered log never loads silently. Re-measured the same way (median of 3):

| Size | Before the hash chain | With the hash chain | Change |
|---|---|---|---|
| 10k | 5.56 ms | 6.68 ms | +20% |
| 100k | 55.1 ms | 69.3 ms | +26% |
| 1M | 542 ms | 679 ms | +25% |

Replay also makes one more short-lived allocation per record: `ChainHash` builds its input in a fresh buffer, which could be avoided later. Other operations add one SHA-256 of about 100 bytes per record written, too small to see next to an fsync. They were not re-measured.

### With redaction

`Read` now holds its lock through the disk read, so a segment rewrite never swaps a file under it. `Get` was re-measured (median of 3) to check that this costs nothing: 1.22 / 1.37 / 1.53 µs at 10k / 100k / 1M, against 1.27 / 1.45 / 1.50 µs before. The paths that write and recall are unchanged. Redaction itself (a segment rewrite) is not benchmarked yet.

### Recall walk alone (cheesedb, in memory, no disk)

| Size | removes 20 | removes 10,000 |
|---|---|---|
| 10k | 1.09 µs | 106 µs |
| 100k | 1.13 µs | 111 µs |
| 1M | 1.32 µs | 109 µs |

A full `Recall` is one durable write plus this walk. The "removes 10,000" durable figure above is computed this way (4.94 ms + 0.11 ms) rather than measured, because setting up a 10,000-memory probe through the API costs 10,001 fsyncs per run.

### Space

| | Size | cheesedb | SQLite | PostgreSQL |
|---|---|---|---|---|
| Disk per memory | 10k | 312 B | 233 B | 561 B |
| | 100k | 314 B | 245 B | 546 B |
| | 1M | 314 B | 249 B | 546 B |
| RAM held while open, per memory | 1M | 205 B (about 205 MB in total) | page cache only | shared buffers only |

## What this shows

**Where cheesedb wins**
- **Reads by id are about 4-5x faster than SQLite and about 30x faster than PostgreSQL.** cheesedb needs one array lookup and one `pread`. SQLite walks a B-tree through the SQL layer. PostgreSQL also pays a network round trip (see "Caveats").
- **Large recalls are about 45x faster than SQLite and about 340x faster than PostgreSQL.** cheesedb walks the graph in memory (about 11 ns per memory removed), then writes once. The SQL versions run 3-4 statements per affected memory, about 40,000 statements for a 10,000-memory recall. PostgreSQL pays a round trip for each.
- **Small recalls are about 1.4-1.5x faster than SQLite and 2x faster than PostgreSQL.**

**Where it is about even**
- **Durable writes.** All three cost about 5-6 ms, because one `F_FULLFSYNC` on this SSD takes about 4.5 ms and dominates everything else. cheesedb is about 10% faster than SQLite and 25-30% faster than PostgreSQL. That is roughly 200 writes per second per agent for any of them.
  - Group commit does not help within one agent, because writes to one agent go one at a time.
  - Different agents write in parallel.
- **Recall cost does not grow with ledger size, in all three engines.** The SQL versions are indexed by parent, so they are proportional to the work too. cheesedb's walk stays at about 1.1-1.3 µs for 20 memories from 10k to 1M, but this property is not unique to cheesedb.

**Where cheesedb loses**
- **Startup.** cheesedb must replay its entire log on open: 542 ms at 1M memories (679 ms now that every open checks the hash chain), growing in a straight line with history. SQLite is ready in 0.1 ms. This is the main known weakness (a known, accepted weakness). v3 snapshots are the planned fix. Replay also allocates about 1 GB of short-lived memory at 1M.
- **RAM.** cheesedb keeps the whole graph in memory: about 205 bytes per memory, or about 205 MB for 1M. SQLite and PostgreSQL keep only what their caches hold, so they can serve ledgers much larger than RAM. cheesedb cannot.
- **Disk vs SQLite.** cheesedb uses about 26% more disk than SQLite (314 B vs 249 B per memory). The fixed record header is 158 bytes plus a 16-byte salt, including 72 bytes reserved for v2 signatures and keys. Compared with PostgreSQL, it uses about 42% less.

## Caveats

- **One machine, one run of 3.** Durable write numbers depend almost entirely on the disk's flush speed. Expect different absolute numbers elsewhere.
- **The OS file cache was warm.** All datasets fit in RAM, so reads never waited for the SSD in any engine.
- **PostgreSQL runs over TCP on localhost.** It was reached that way because Unix socket paths are length-limited and the temp folder path is long. A Unix socket would shave some microseconds off each round trip. Being client/server is still part of what PostgreSQL costs here.
- **The SQL recall is a direct port of cheesedb's algorithm,** one statement at a time. A SQL expert could batch more work per statement, for example a recursive CTE for Strict recall. That would narrow the large-recall gap but not close it. Fair recall's leg counting does not map neatly onto one set-based query.
- **Single client.** These numbers say nothing about many concurrent agents or connections.
- **The datasets grow during the run.** Remember and Recall add records to the shared datasets; read-only benchmarks run first. The effect is small (a few thousand records).
- **The workload is synthetic.** Real agents have different leg shapes and content sizes. Larger content would make every engine's per-memory disk use converge.
- **Not measured:** writes without fsync (dropped by decision), concurrent writers, Strict recall, and reads from a cold cache.

## How to reproduce

cheesedb (main module, no dependencies):

```
go test -run '^$' -bench . -benchmem -count=3 -timeout 60m ./internal/ledger/
```

SQLite and PostgreSQL (`/bench` module):

```
bench/pg.sh start /some/temp/dir              # throwaway PostgreSQL on 127.0.0.1:54329
cd bench
CHEESEDB_PG="host=127.0.0.1 port=54329 user=$USER" \
  go test -run '^$' -bench . -benchmem -count=3 -timeout 90m
cd ..
bench/pg.sh stop /some/temp/dir               # then delete the folder
```

Without `CHEESEDB_PG`, the PostgreSQL benchmarks are skipped and only SQLite runs.
