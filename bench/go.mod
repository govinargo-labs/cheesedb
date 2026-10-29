module github.com/govinargo-labs/cheesedb/bench

go 1.26.5

require (
	github.com/jackc/pgx/v5 v5.11.0
	github.com/mattn/go-sqlite3 v1.14.52
	github.com/govinargo-labs/cheesedb v0.0.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)

// The main module, from this repository, so the benchmarks share its
// workload generator (internal/benchgen).
replace github.com/govinargo-labs/cheesedb => ../
