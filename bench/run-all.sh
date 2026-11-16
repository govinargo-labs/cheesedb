#!/bin/sh
# Runs every benchmark behind BENCHMARKS.md and keeps the raw output, so
# each published number can be traced to the run that produced it.
#
#   bench/run-all.sh OUT SCRATCH
#
# OUT gets one text file per benchmark command, unedited, plus env.txt
# (commit, machine, versions, the sha256 of each test binary). SCRATCH holds
# the throwaway PostgreSQL and Neo4j servers and is deleted at the end; it
# needs about 10 GB. PostgreSQL and Neo4j come from Homebrew (bench/pg.sh,
# bench/neo4j.sh); without them those parts are skipped, and env.txt says so.
#
# Run it on an idle machine: durable writes measure the disk's flush time,
# and anything else using the disk or CPU shows up in the numbers. It takes
# a few hours.
set -eu
mkdir -p "$1"
out=$(cd "$1" && pwd)
scratch=$2
root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$scratch"
export TMPDIR="$scratch/tmp"
mkdir -p "$TMPDIR"

# Test binaries are built once, so every run uses the same code, and their
# hashes go into env.txt.
cd "$root"
go test -c -o "$scratch/ledger.test" ./internal/ledger/
(cd bench && go test -c -o "$scratch/bench.test" .)

{
	echo "commit: $(git rev-parse HEAD) ($(git describe --tags --always --dirty))"
	echo "changed files: $(git status --porcelain | tr '\n' ' ')"
	echo "go: $(go version)"
	echo "os: $(uname -srm)"
	[ "$(uname)" = Darwin ] && echo "macOS: $(sw_vers -productVersion)" &&
		echo "cpu: $(sysctl -n machdep.cpu.brand_string), $(sysctl -n hw.ncpu) cores" &&
		echo "memory: $(($(sysctl -n hw.memsize) / 1073741824)) GB"
	echo "postgres: $(/opt/homebrew/opt/postgresql@17/bin/postgres --version 2>/dev/null || echo 'not installed: skipped')"
	echo "neo4j: $(/opt/homebrew/opt/neo4j/bin/neo4j --version 2>/dev/null || echo 'not installed: skipped')"
	echo "sha256:"
	shasum -a 256 "$scratch/ledger.test" "$scratch/bench.test" | sed "s|$scratch/||"
} >"$out/env.txt"

run() { # run NAME DIR COMMAND...: one benchmark command into OUT/NAME.txt
	name=$1 dir=$2
	shift 2
	echo "== $name"
	# The command line is written with $SCRATCH and $USER in place of the
	# local folder and account name, which do not belong in published files.
	(cd "$dir" && echo "\$ $*" | sed "s|$scratch|\$SCRATCH|g; s|user=$(id -un)|user=\$USER|g" && "$@") >"$out/$name.txt" 2>&1
}

# cheesedb: every operation at 10k, 100k and 1M memories.
run cheesedb "$root/internal/ledger" "$scratch/ledger.test" \
	-test.run '^$' -test.bench . -test.benchmem -test.count=3 -test.timeout 60m

# cheesedb: recall of 10,000 memories, end to end (about 48 s of setup per run).
run cheesedb-recall-large "$root/internal/ledger" env CHEESEDB_BENCH_LARGE=1 "$scratch/ledger.test" \
	-test.run '^$' -test.bench RecallLarge -test.benchtime=1x -test.count=3

# cheesedb: the search index, 1,000 to 1M memories, then peak process memory
# for each size alone (the test binary itself, not the go command).
run cheesedb-index "$root/internal/ledger" env CHEESEDB_BENCH_INDEX=1 "$scratch/ledger.test" \
	-test.run '^$' -test.bench Index -test.benchtime=3x -test.count=3 -test.benchmem -test.timeout 90m
for size in 1k 10k 100k 1M; do
	run "cheesedb-index-peak-$size" "$root/internal/ledger" env CHEESEDB_BENCH_INDEX=1 /usr/bin/time -l "$scratch/ledger.test" \
		-test.run '^$' -test.bench "Index/$size/" -test.benchtime=3x -test.count=1
done

# SQLite always; PostgreSQL on a throwaway server when installed.
pg=""
if [ -x /opt/homebrew/opt/postgresql@17/bin/pg_ctl ]; then
	"$root/bench/pg.sh" start "$scratch/pg" >/dev/null
	pg="host=127.0.0.1 port=54329 user=$(id -un)"
fi
run sqlite-postgres "$root/bench" env CHEESEDB_PG="$pg" "$scratch/bench.test" \
	-test.run '^$' -test.bench . -test.benchmem -test.count=3 -test.timeout 180m
[ -n "$pg" ] && "$root/bench/pg.sh" stop "$scratch/pg" >/dev/null && rm -rf "$scratch/pg"

# Neo4j at 1M on a fresh server, then its disk use after a clean stop, then
# the check that its recall matches SQL on another fresh server.
if [ -x /opt/homebrew/opt/neo4j/bin/neo4j ]; then
	"$root/bench/neo4j.sh" start "$scratch/neo4j" >/dev/null
	run neo4j "$root/bench" env CHEESEDB_NEO4J=bolt://127.0.0.1:7699 "$scratch/bench.test" \
		-test.run '^$' -test.bench 'Neo4j//1M' -test.benchmem -test.count=3 -test.timeout 240m
	"$root/bench/neo4j.sh" stop "$scratch/neo4j" >/dev/null
	rm -rf "$scratch/neo4j"
	# Disk use: a fresh server loaded with only the 1M dataset (one Get
	# run loads it), stopped cleanly so the store files are complete.
	"$root/bench/neo4j.sh" start "$scratch/neo4j-disk" >/dev/null
	run neo4j-load "$root/bench" env CHEESEDB_NEO4J=bolt://127.0.0.1:7699 "$scratch/bench.test" \
		-test.run '^$' -test.bench 'Neo4jGet/neo4j/1M' -test.benchtime=1x -test.count=1
	"$root/bench/neo4j.sh" stop "$scratch/neo4j-disk" >/dev/null
	(echo '$ du -sk data/databases/neo4j' && cd "$scratch/neo4j-disk" && du -sk data/databases/neo4j) >"$out/neo4j-disk.txt"
	rm -rf "$scratch/neo4j-disk"
	"$root/bench/neo4j.sh" start "$scratch/neo4j-check" >/dev/null
	run neo4j-matches-sql "$root/bench" env CHEESEDB_NEO4J=bolt://127.0.0.1:7699 "$scratch/bench.test" \
		-test.run TestNeo4jRecallMatchesSQLite -test.v
	"$root/bench/neo4j.sh" stop "$scratch/neo4j-check" >/dev/null
	rm -rf "$scratch/neo4j-check"
fi

rm -rf "$scratch"
echo "done: $out"
