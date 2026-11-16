# cheesedb benchmarks

Each number is the **median of 3 runs** (`-count=3`). Every table comes from one run of v0.3.0, unless it says otherwise. **The raw output of that run is kept unedited in [`bench/results/v0.3.0/`](bench/results/v0.3.0/)**, one file per command, with the commit, machine, versions and the checksum of each test program in `env.txt`. `bench/run-all.sh` repeats the whole run with one command (see "How to reproduce").

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
| Neo4j | Community 2026.10.2 (Homebrew) on OpenJDK 21.0.13, over Bolt on 127.0.0.1, through `github.com/neo4j/neo4j-go-driver/v6` v6.4.0; 2 GB heap and 4 GB page cache, set in `bench/neo4j.sh` |

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
| **Recall**, removes 10,000 memories (durable) | **5.23 ms** | 236 ms | 1.94 s | 67.7 ms |
| **Recall**, removes 20 memories (durable) | **4.89 ms** | 7.24 ms | 10.1 ms | 6.54 ms |
| **Get** (one memory by id) | **1.75 µs** | 7.38 µs | 59.3 µs | 211 µs |
| **Remember** (one memory, durable) | **4.63 ms** | 5.55 ms | 6.77 ms | 6.09 ms |
| **Startup**: ready to answer the first query | 706 ms | **0.10 ms** | 2.99 ms | 1.16 ms¹ |
| Disk per memory | 314 B | **249 B** | 546 B | 496 B² |
| RAM | 205 B per memory, in the process | page cache | shared buffers (128 MB) | JVM, 2 GB heap and 4 GB page cache set; 3.2 GB resident after the run |

- The cheesedb "removes 10,000" figure is **measured end to end** (`BenchmarkRecallLarge`): 10,001 durable writes build the probe, then one `Recall` is timed. The three runs were 5.05, 5.23 and 5.86 ms.
- The three runs of the other engines' 10,000-memory recall were 235 to 236 ms (SQLite), 1.90 to 2.24 s (PostgreSQL) and 67.7 to 67.8 ms (Neo4j).
- ¹ A new connection to a Neo4j server that is already running, like the PostgreSQL figure. It does not include booting the server, which took 6.5 to 8.2 s with this dataset in an earlier run. The three runs were 1.70, 1.16 and 0.84 ms (the JVM warms up).
- ² The store files only, after a clean shutdown, on a freshly loaded 1M dataset (1.11M nodes, 1.50M legs, 3.75M relationships): 483,896 KB (`neo4j-disk.txt`). The transaction log is not counted, as the write-ahead log is not counted for the others.
- During the Neo4j run the graph grew from 1.11M to 1.72M nodes, because each 10,000-memory recall needs a fresh probe. The recall time did not change across the three runs while it grew.

Neo4j was run at 1M only. The two tables below have the other three engines at three sizes, from the same run.

### Speed per operation

| Operation | Size | cheesedb | SQLite | PostgreSQL |
|---|---|---|---|---|
| **Remember** (one memory, durable) | 10k | 4.60 ms | 4.95 ms | 6.10 ms |
| | 100k | 4.48 ms | 5.15 ms | 6.28 ms |
| | 1M | 4.63 ms | 5.55 ms | 6.77 ms |
| **Get** (one memory by id) | 10k | 1.37 µs | 5.57 µs | 52.4 µs |
| | 100k | 1.58 µs | 6.93 µs | 53.5 µs |
| | 1M | 1.75 µs | 7.38 µs | 59.3 µs |
| **Recall**, removes 20 memories (durable) | 10k | 4.70 ms | 7.12 ms | 10.3 ms |
| | 100k | 4.71 ms | 7.12 ms | 9.92 ms |
| | 1M | 4.89 ms | 7.24 ms | 10.1 ms |
| **Recall**, removes 10,000 memories (durable) | 1M | 5.23 ms (measured end to end, see below) | 236 ms | 1.94 s |
| **Startup**: ready to answer the first query | 10k | 7.16 ms | 0.10 ms | 2.88 ms |
| | 100k | 74.5 ms | 0.10 ms | 3.72 ms |
| | 1M | **706 ms** | 0.10 ms | 2.99 ms |

### Startup with the hash chain (checked on every Open)

Replay recomputes every record's chain hash, so a tampered log never loads silently. From an earlier run that measured startup before and after the hash chain was added (median of 3); the v0.3.0 run above has 7.16 ms, 74.5 ms and 706 ms:

| Size | Before the hash chain | With the hash chain | Change |
|---|---|---|---|
| 10k | 5.56 ms | 6.68 ms | +20% |
| 100k | 55.1 ms | 69.3 ms | +26% |
| 1M | 542 ms | 679 ms | +25% |

Replay also makes one more short-lived allocation per record: `ChainHash` builds its input in a fresh buffer, which could be avoided later. Other operations add one SHA-256 of about 100 bytes per record written, too small to see next to an fsync. They were not re-measured.

### With redaction

From an earlier run. `Read` now holds its lock through the disk read, so a segment rewrite never swaps a file under it. `Get` was re-measured (median of 3) to check that this costs nothing: 1.22 / 1.37 / 1.53 µs at 10k / 100k / 1M, against 1.27 / 1.45 / 1.50 µs before. The paths that write and recall are unchanged. Redaction itself (a segment rewrite) is not benchmarked yet.

### Recall walk alone (cheesedb, in memory, no disk)

| Size | removes 20 | removes 10,000 |
|---|---|---|
| 10k | 1.68 µs | 182 µs |
| 100k | 1.76 µs | 161 µs |
| 1M | 1.38 µs | 169 µs |

A full `Recall` is one durable write plus this walk. The "removes 10,000" durable figure above is measured end to end by `BenchmarkRecallLarge`, which only runs when asked (see "How to reproduce"), because setting up a 10,000-memory probe through the API costs 10,001 fsyncs (about 48 s) per run.

The walk measured slower in this run than in the earlier one (1.1-1.3 µs and 106-111 µs). The walk itself did not change; the cause has not been investigated. A durable recall is dominated by the flush either way (about 5 ms).

### Space

| | Size | cheesedb | SQLite | PostgreSQL |
|---|---|---|---|---|
| Disk per memory | 10k | 312 B | 233 B | 561 B |
| | 100k | 314 B | 245 B | 546 B |
| | 1M | 314 B | 249 B | 546 B |
| RAM held while open, per memory | 1M | 205 B (about 205 MB in total) | page cache only | shared buffers only |

## Search index: cheesedb's own cost, by size

Same machine. These numbers are cheesedb alone: **no embedding model is involved**. Each memory gets a 768-value vector (random values, which cost exactly what real ones cost: search compares every value of every vector, whatever they are). The graph is the same generated workload as above. Median of 3 runs of `BenchmarkIndex` (see "How to reproduce"); peak process memory is the "peak memory footprint" that `/usr/bin/time -l` reports for one run per size of the test program alone, building the dataset included.

In memory, each vector is kept at one byte per value; a search scans those, then reads the exact float32 vectors of the few rows that could still be in the top results from disk, so its answer is the same as an exact float32 scan ([ARCHITECTURE.md section 9](ARCHITECTURE.md#9-vector-indexes)).

| Memories in one agent | 1,000 | 10,000 | 100,000 | 1,000,000 |
|---|---|---|---|---|
| Disk: memories / search index | 0.3 MB / 3.1 MB | 3 MB / 31 MB | 30 MB / 311 MB | 300 MB / 3.1 GB |
| RAM with the index loaded (graph included) | 1.4 MB | 11 MB | 99 MB | 1.1 GB |
| Peak process memory | 17 MB | 58 MB | 420 MB | 4.4 GB |
| **Search, store kept open** | **1.1 ms** | **1.9 ms** | **15 ms** | **143 ms** |
| Search from a fresh open (open, load the index, search) | 9.0 ms | 79 ms | 0.74 s | 7.5 s |
| Recall with Erase, store kept open | 15 ms | 51 ms | 80 ms | 164 ms |
| Recall with Erase from a fresh open | 31 ms | 112 ms | 0.81 s | 7.5 s |
| Recall Untrusted from a fresh open | 5.9 ms | 12 ms | 0.33 s | 0.71 s |
| Store every memory's vector | 60 ms | 0.42 s | 4.2 s | 42 s |

"From a fresh open" is what each `cheesedb` command and each MCP tool call does, because they open the store for every call. A Go program that keeps the store open pays the load once.

**One byte per value, against the float32 index it replaced** (same benchmark, medians; the float32 column is from an earlier run):

| | float32 in RAM | one byte in RAM |
|---|---|---|
| RAM with the index loaded, 1M | 3.6 GB | 1.1 GB |
| Peak process memory, 1M | 12 GB | 4.4 GB |
| Search, store kept open, 1k / 1M | 0.9 ms / 163 ms | 1.1 ms / 143 ms |
| Search from a fresh open, 100k / 1M | 0.68 s / 10.6 s | 0.74 s / 7.5 s |

Small indexes pay a few exact reads from disk per search (about 0.2 ms); large ones gain from scanning a quarter of the bytes. Disk is unchanged: the files keep float32.

**The models, measured separately** (local oMLX on the same Mac; they are not part of cheesedb, and any other server will differ):

| | |
|---|---|
| Embedding model, one question (nomic ModernBERT, 4-bit) | 17 ms |
| Embedding model, 1M memories (`cheesedb embed`) | about 36 minutes (455 texts per second); cheesedb's part is the 46 s above |
| Chat model, one `ask` answer (Qwen3.6-35B-A3B, 4-bit) | a few seconds |

**With real embeddings at 1M.** The same 1M-memory size was also run end to end with the real embedding model and varied text (1,000 lots and 999,000 memories about people, cities, foods and restaurants, tens of thousands about satay or peanuts, plus the peanut story from the [README](README.md#quick-start)). "Can the user eat the satay at Spice Garden?" ranked the two right memories first (0.82 and 0.77), ahead of "Grace from Lima likes chicken satay" (0.67). After `recall ... erase` of the restaurant's web page, neither came back, and `ask` answered "I do not know". The command's search took 7 to 10 s with the float32 index of the time, of which 17 ms was the embedding model.

**What this shows**
- **Search with the store open stays fast:** 15 ms at 100,000 memories, 143 ms at 1M. The rest of the graph (read by id, recall) does not depend on the index.
- **The cost is loading the index, not the model.** A fresh open reads the whole index log: under 0.1 s up to 10,000 memories, 0.74 s at 100,000, 7.5 s at 1M. The `cheesedb` command and the MCP server pay it on every search and every erase, so they suit agents of up to about 100,000 memories.
- **Loading is still wasteful:** at 1M, the process peaks at 4.4 GB while the loaded index and graph take 1.1 GB.
- Planned: keep the index loaded across MCP calls, a lighter load, and writing vectors as they are embedded.

## What this shows

**Where cheesedb wins**
- **Reads by id are about 4x faster than SQLite, about 30x faster than PostgreSQL and about 120x faster than Neo4j.** cheesedb needs one array lookup and one `pread`. SQLite walks a B-tree through the SQL layer. PostgreSQL and Neo4j also pay a network round trip (see "Caveats"); Neo4j's 211 µs is mostly that round trip.
- **Large recalls are about 45x faster than SQLite, about 370x faster than PostgreSQL and about 13x faster than Neo4j.** cheesedb walks the graph in memory (about 17 ns per memory removed), then writes once. The SQL versions run 3-4 statements per affected memory, about 40,000 statements for a 10,000-memory recall; PostgreSQL pays a round trip for each. Neo4j, with the recall written as a few set-based queries, does it about 3.5x faster than SQLite, and is still 13x slower than cheesedb.
- **Small recalls are about 1.3-1.5x faster than SQLite and Neo4j and 2x faster than PostgreSQL.**

**Where it is about even**
- **Durable writes.** All four cost about 5-7 ms, because one `F_FULLFSYNC` on this SSD takes about 4.5 ms and dominates everything else. cheesedb is about 20% faster than SQLite and 30-45% faster than Neo4j and PostgreSQL. That is roughly 200 writes per second per agent for any of them.
  - Group commit does not help within one agent, because writes to one agent go one at a time.
  - Different agents write in parallel.
- **Recall cost does not grow with ledger size, in the engines measured at several sizes.** The SQL versions are indexed by parent, so they are proportional to the work too. cheesedb's walk stays at about 1.4-1.8 µs for 20 memories from 10k to 1M, but this property is not unique to cheesedb. Neo4j was measured at 1M only; its recall time did not change while its graph grew by about 50% during the run.

**Where cheesedb loses**
- **Startup.** cheesedb must replay its entire log on open, checking the hash chain as it goes: 706 ms at 1M memories, growing in a straight line with history. SQLite is ready in 0.1 ms. PostgreSQL and Neo4j answer a new connection in 3 ms and 1.2 ms, but they are servers that were already running: booting Neo4j with this dataset took 6.5 to 8.2 s in an earlier run. This is the main known weakness (a known, accepted weakness). v3 snapshots are the planned fix. Replay also allocates about 1 GB of short-lived memory at 1M.
- **RAM.** cheesedb keeps the whole graph in memory: about 205 bytes per memory, or about 205 MB for 1M. SQLite and PostgreSQL keep only what their caches hold, so they can serve ledgers much larger than RAM. cheesedb cannot.
- **Disk vs SQLite.** cheesedb uses about 26% more disk than SQLite (314 B vs 249 B per memory). The fixed record header is 158 bytes plus a 16-byte salt, including 72 bytes reserved for v2 signatures and keys. Compared with PostgreSQL, it uses about 42% less, and compared with Neo4j (496 B) about 37% less.

## Caveats

- **One machine, one run of 3.** The raw output is in `bench/results/v0.3.0/`. Durable write numbers depend almost entirely on the disk's flush speed. Expect different absolute numbers elsewhere.
- **The OS file cache was warm.** All datasets fit in RAM, so reads never waited for the SSD in any engine.
- **PostgreSQL runs over TCP on localhost.** It was reached that way because Unix socket paths are length-limited and the temp folder path is long. A Unix socket would shave some microseconds off each round trip. Being client/server is still part of what PostgreSQL costs here.
- **The SQL recall is a direct port of cheesedb's algorithm,** one statement at a time. A SQL expert could batch more work per statement, for example a recursive CTE for Strict recall. That would narrow the large-recall gap but not close it. Fair recall's leg counting does not map neatly onto one set-based query.
- **Single client.** These numbers say nothing about many concurrent agents or connections.
- **The Neo4j model and queries are ours.** We modelled the ledger as a graph and wrote the recall as set-based Cypher for this job. A Neo4j expert may find a faster way, so treat Neo4j's numbers as what a careful developer gets without tuning. Neo4j also runs over Bolt on localhost, like PostgreSQL, and the 2 GB heap and 4 GB page cache are our choice; Community Edition has one user database, so each server holds one dataset size.
- **The datasets grow during the run.** Remember and Recall add records to the shared datasets; read-only benchmarks run first. The effect is a few thousand records, except for the 10,000-memory recall, which adds 10,001 records per run (for Neo4j, about 0.6M in all, as above).
- **The workload is synthetic.** Real agents have different leg shapes and content sizes. Larger content would make every engine's per-memory disk use converge.
- **Not measured:** writes without fsync (dropped by decision), concurrent writers, Strict recall, and reads from a cold cache.

## How to reproduce

**Everything at once**, as for `bench/results/v0.3.0/` (a few hours on an idle machine; needs about 10 GB of scratch space, and PostgreSQL 17 and Neo4j from Homebrew for those engines):

```
bench/run-all.sh bench/results/my-run /some/temp/dir
```

It builds each test program once, runs every command below, keeps each command's raw output in its own file, records the commit, machine, versions and program checksums in `env.txt`, and deletes the temp folder at the end. The commands it runs, one by one:

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

The search index at 1,000 to 1M memories (slow: building the 1M dataset takes about a minute and 3.4 GB of temporary disk):

```
CHEESEDB_BENCH_INDEX=1 go test -run '^$' -bench Index -benchtime=3x -count=3 -benchmem -timeout 90m ./internal/ledger/
```

Peak process memory comes from running that command under `/usr/bin/time -l` (macOS) or `/usr/bin/time -v` (Linux), one size at a time (`-bench 'Index/1M/'`).
