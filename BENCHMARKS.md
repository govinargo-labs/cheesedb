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
| Neo4j | Community 2026.10.1 (Homebrew) on OpenJDK 21.0.13, over Bolt on 127.0.0.1, through `github.com/neo4j/neo4j-go-driver/v6` v6.4.0; 2 GB heap and 4 GB page cache, set in `bench/neo4j.sh` |

## What is compared

All four engines run the **same workload** (`internal/benchgen`), built from a fixed seed:
- every 10th record is a lot;
- each memory has 1-2 legs of 1-2 parents, chosen from the previous 1,000 records;
- every lot and memory holds 100 bytes of content.

Sizes: 10k, 100k and 1M memories (plus about 11% more records as lots).

**Same durability everywhere.** A write is acknowledged only after it is flushed to the physical disk with `F_FULLFSYNC`, the strongest flush on macOS:
- cheesedb: Go's `File.Sync` uses `F_FULLFSYNC` on macOS;
- SQLite: WAL mode, `synchronous=FULL`, `fullfsync=ON`;
- PostgreSQL: `fsync=on`, `synchronous_commit=on`, `wal_sync_method=fsync_writethrough`;
- Neo4j: forces its transaction log to disk before it acknowledges a commit, and has no setting to turn that off. We checked that it is a full flush on this Mac: a one-statement write costs 4.8 ms more than a one-statement read (5.13 ms against 0.31 ms), the same as cheesedb's own flush (about 4.8 ms).

**How the SQL versions work.** They store the ledger the way a developer would on a general database:
- tables `nodes`, `legs` and `leg_parents` (indexed by parent), plus `recalls`;
- the same counter algorithm cheesedb uses (`dead_parents`, `alive_legs`), run as SQL statements in one transaction per operation;
- one client connection.

They are a straightforward port, not hand-tuned SQL. See "Caveats".

**How the Neo4j version works.** A graph database holds the ledger as a graph, `(parent)-[:IN_LEG]->(leg)-[:OF]->(memory)`, where a leg is a node because it joins several parents (all needed) to one memory. Recall is written the graph way, with no counters: one set-based Cypher query per level of the cascade, where a memory dies when none of its legs has all its parents alive. That is a different algorithm from the SQL port, and a more natural one for a graph. Its result is checked against the SQL recall: `TestNeo4jRecallMatchesSQLite` runs 40 random recalls over a dataset with deep cascades, and both engines must kill exactly the same memories (we also broke the query on purpose twice, and the test caught both).

## Results

### All four engines at 1M memories

One consistent run, each engine alone on the idle machine, median of 3 runs.

| Operation | cheesedb | SQLite | PostgreSQL | Neo4j |
|---|---|---|---|---|
| **Recall**, removes 10,000 memories (durable) | **5.49 ms** | 235 ms | 1.82 s | 67.1 ms |
| **Recall**, removes 20 memories (durable) | **4.84 ms** | 7.12 ms | 10.5 ms | 6.84 ms |
| **Get** (one memory by id) | **1.57 µs** | 7.02 µs | 44.3 µs | 179 µs |
| **Remember** (one memory, durable) | **4.65 ms** | 5.26 ms | 6.61 ms | 6.26 ms |
| **Startup**: ready to answer the first query | 667 ms | **0.10 ms** | 2.73 ms | 1.03 ms¹ |
| Disk per memory | 314 B | **249 B** | 546 B | 496 B² |
| RAM | 205 B per memory, in the process | page cache | shared buffers (128 MB) | JVM, 2 GB heap and 4 GB page cache set; 3.2 GB resident after the run |

- The cheesedb "removes 10,000" figure is **measured end to end** (`BenchmarkRecallLarge`): 10,001 durable writes build the probe, then one `Recall` is timed. The three runs were 5.62, 4.09 and 5.49 ms. Earlier versions of this file computed it from two measured parts (a durable write plus the in-memory walk, about 5.05 ms); the measurement agrees.
- The three runs of the other engines' 10,000-memory recall were 233 to 237 ms (SQLite), 1.82 to 1.84 s (PostgreSQL) and 66.5 to 68.2 ms (Neo4j).
- ¹ A new connection to a Neo4j server that is already running, like the PostgreSQL figure. It does not include booting the server, which took 6.5 to 8.2 s with this dataset. The three runs were 1.61, 1.03 and 0.73 ms (the JVM warms up).
- ² The store files only, after a clean shutdown, on a freshly loaded 1M dataset (1.11M nodes, 1.50M legs, 3.75M relationships). The transaction log is not counted, as the write-ahead log is not counted for the others.
- During the Neo4j run the graph grew from 1.11M to 1.72M nodes, because each 10,000-memory recall needs a fresh probe. The recall time did not change across the three runs while it grew.

The tables below come from earlier runs on three engines at three sizes; the 1M results above match them within a few percent.

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
| **Recall**, removes 10,000 memories (durable) | 1M | 5.49 ms (measured end to end, see below) | 235 ms | 1.82 s |
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

A full `Recall` is one durable write plus this walk. The "removes 10,000" durable figure above was first computed this way (4.94 ms + 0.11 ms), because setting up a 10,000-memory probe through the API costs 10,001 fsyncs (about 48 s) per run. It is now also measured end to end by `BenchmarkRecallLarge`, which only runs when asked (see "How to reproduce"): 5.49 ms, with runs of 5.62, 4.09 and 5.49 ms, against 5.05 ms computed.

### Space

| | Size | cheesedb | SQLite | PostgreSQL |
|---|---|---|---|---|
| Disk per memory | 10k | 312 B | 233 B | 561 B |
| | 100k | 314 B | 245 B | 546 B |
| | 1M | 314 B | 249 B | 546 B |
| RAM held while open, per memory | 1M | 205 B (about 205 MB in total) | page cache only | shared buffers only |

## What this shows

**Where cheesedb wins**
- **Reads by id are about 4-5x faster than SQLite, about 30x faster than PostgreSQL and about 110x faster than Neo4j.** cheesedb needs one array lookup and one `pread`. SQLite walks a B-tree through the SQL layer. PostgreSQL and Neo4j also pay a network round trip (see "Caveats"); Neo4j's 179 µs is mostly that round trip.
- **Large recalls are about 43x faster than SQLite, about 330x faster than PostgreSQL and about 12x faster than Neo4j.** cheesedb walks the graph in memory (about 11 ns per memory removed), then writes once. The SQL versions run 3-4 statements per affected memory, about 40,000 statements for a 10,000-memory recall; PostgreSQL pays a round trip for each. Neo4j, with the recall written as a few set-based queries, does it about 3.5x faster than SQLite, and is still 12x slower than cheesedb.
- **Small recalls are about 1.4-1.5x faster than SQLite and Neo4j and 2x faster than PostgreSQL.**

**Where it is about even**
- **Durable writes.** All four cost about 5-7 ms, because one `F_FULLFSYNC` on this SSD takes about 4.5 ms and dominates everything else. cheesedb is about 10% faster than SQLite and 25-30% faster than PostgreSQL and Neo4j. That is roughly 200 writes per second per agent for any of them.
  - Group commit does not help within one agent, because writes to one agent go one at a time.
  - Different agents write in parallel.
- **Recall cost does not grow with ledger size, in the engines measured at several sizes.** The SQL versions are indexed by parent, so they are proportional to the work too. cheesedb's walk stays at about 1.1-1.3 µs for 20 memories from 10k to 1M, but this property is not unique to cheesedb. Neo4j was measured at 1M only; its recall time did not change while its graph grew by about 50% during the run.

**Where cheesedb loses**
- **Startup.** cheesedb must replay its entire log on open: 542 ms at 1M memories (679 ms now that every open checks the hash chain), growing in a straight line with history. SQLite is ready in 0.1 ms. PostgreSQL and Neo4j answer a new connection in 3 ms and 1 ms, but they are servers that were already running: booting Neo4j with this dataset took 6.5 to 8.2 s. This is the main known weakness (a known, accepted weakness). v3 snapshots are the planned fix. Replay also allocates about 1 GB of short-lived memory at 1M.
- **RAM.** cheesedb keeps the whole graph in memory: about 205 bytes per memory, or about 205 MB for 1M. SQLite and PostgreSQL keep only what their caches hold, so they can serve ledgers much larger than RAM. cheesedb cannot.
- **Disk vs SQLite.** cheesedb uses about 26% more disk than SQLite (314 B vs 249 B per memory). The fixed record header is 158 bytes plus a 16-byte salt, including 72 bytes reserved for v2 signatures and keys. Compared with PostgreSQL, it uses about 42% less, and compared with Neo4j (496 B) about 37% less.

## Caveats

- **One machine, one run of 3.** Durable write numbers depend almost entirely on the disk's flush speed. Expect different absolute numbers elsewhere.
- **The OS file cache was warm.** All datasets fit in RAM, so reads never waited for the SSD in any engine.
- **PostgreSQL runs over TCP on localhost.** It was reached that way because Unix socket paths are length-limited and the temp folder path is long. A Unix socket would shave some microseconds off each round trip. Being client/server is still part of what PostgreSQL costs here.
- **The SQL recall is a direct port of cheesedb's algorithm,** one statement at a time. A SQL expert could batch more work per statement, for example a recursive CTE for Strict recall. That would narrow the large-recall gap but not close it. Fair recall's leg counting does not map neatly onto one set-based query.
- **Single client.** These numbers say nothing about many concurrent agents or connections.
- **The Neo4j model and queries are ours.** We modelled the ledger as a graph and wrote the recall as set-based Cypher for this job. A Neo4j expert may find a faster way, so treat Neo4j's numbers as what a careful developer gets without tuning. Neo4j also runs over Bolt on localhost, like PostgreSQL, and the 2 GB heap and 4 GB page cache are our choice; Community Edition has one user database, so each server holds one dataset size.
- **The datasets grow during the run.** Remember and Recall add records to the shared datasets; read-only benchmarks run first. The effect is a few thousand records, except for the 10,000-memory recall, which adds 10,001 records per run (for Neo4j, about 0.6M in all, as above).
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

To run only the 1M cases of the `/bench` module, use `-bench '//1M'` (the engine is the second level of each name, the size the third).

Neo4j (`/bench` module; needs `brew install neo4j`, which brings Java 21):

```
bench/neo4j.sh start /some/temp/dir           # throwaway Neo4j on 127.0.0.1:7699
cd bench
CHEESEDB_NEO4J=bolt://127.0.0.1:7699 \
  go test -run '^$' -bench 'Neo4j//1M' -benchmem -count=3 -timeout 240m
cd ..
bench/neo4j.sh stop /some/temp/dir            # a clean stop flushes the store files
du -sk /some/temp/dir/data/databases/neo4j    # disk use, on a fresh folder loaded with only the dataset
```

A Neo4j server holds one dataset size, so use a fresh folder for each size. The check that its recall matches the SQL recall, which also empties the server, is `CHEESEDB_NEO4J=... go test -run TestNeo4jRecallMatchesSQLite`.

cheesedb's end-to-end recall of 10,000 memories (slow: about 48 s of setup per run, so a fixed count):

```
CHEESEDB_BENCH_LARGE=1 go test -run '^$' -bench RecallLarge -benchtime=1x -count=3 ./internal/ledger/
```
