# Benchmark run: v0.3.0

The raw output behind every table in [BENCHMARKS.md](../../../BENCHMARKS.md), unedited except that each file's first line (the command) shows `$SCRATCH` and `$USER` in place of the local temp folder and account name. Each number in BENCHMARKS.md is the median of the three runs in these files.

| File | What it ran |
|---|---|
| `env.txt` | commit, machine, Go, PostgreSQL and Neo4j versions, and the sha256 of the two test programs used |
| `cheesedb.txt` | cheesedb: open, get, remember, recall and the in-memory recall walk at 10k, 100k and 1M memories |
| `cheesedb-recall-large.txt` | cheesedb: one recall that removes 10,000 memories, end to end |
| `cheesedb-index.txt` | the search index at 1k to 1M memories |
| `cheesedb-index-peak-*.txt` | one index run per size under `/usr/bin/time -l`, for peak process memory |
| `sqlite-postgres.txt` | SQLite and PostgreSQL: the same operations at 10k, 100k and 1M |
| `neo4j.txt` | Neo4j at 1M |
| `neo4j-load.txt`, `neo4j-disk.txt` | Neo4j's disk use: a fresh server loaded with only the 1M dataset, measured after a clean stop |
| `neo4j-matches-sql.txt` | the check that Neo4j's recall removes exactly the memories the SQL version does |

All files come from one run of `bench/run-all.sh` on the machine in `env.txt`, except `neo4j-load.txt` and `neo4j-disk.txt`. In that run the load step used a wrong benchmark name and loaded nothing, so it measured an empty database. Both files come from a second run of that step alone, right after, with the same test program: rebuilt from the same commit, its sha256 matches `env.txt`.

To repeat the run, see "How to reproduce" in BENCHMARKS.md.
